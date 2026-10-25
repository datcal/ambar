#include "AmbarImages.h"

#include "AmbarCache.h"
#include "AmbarClient.h"
#include "IImageWrapper.h"
#include "IImageWrapperModule.h"
#include "Modules/ModuleManager.h"

const FSlateBrush* FAmbarImages::Find(const FString& Key) const
{
	const FEntry* Entry = Entries.Find(Key);
	return Entry ? Entry->Brush.Get() : nullptr;
}

void FAmbarImages::Request(const FString& Key, const FString& Url, const FString& DiskCategory, const FString& DiskKey, bool bNearest)
{
	if (Entries.Contains(Key) || Queued.Contains(Key))
	{
		if (FEntry* Entry = Entries.Find(Key))
		{
			Entry->LastUsed = ++Tick;
		}
		return;
	}
	if (const double* When = Failed.Find(Key))
	{
		// A preview the server has not drawn yet is worth asking about again, but not every frame.
		if (FPlatformTime::Seconds() - *When < 20.0)
		{
			return;
		}
		Failed.Remove(Key);
	}
	Queued.Add(Key);
	Queue.Add(FPending{Key, Url, DiskCategory, DiskKey, bNearest});
	Pump();
}

void FAmbarImages::Reset()
{
	Entries.Reset();
	Failed.Reset();
	Queued.Reset();
	Queue.Reset();
	InFlight = 0;
	++Generation;
}

void FAmbarImages::Pump()
{
	while (InFlight < MaxInFlight && Queue.Num() > 0)
	{
		// Newest first: whatever was scrolled into view last is what the person is looking at.
		const FPending Job = Queue.Pop();

		TArray<uint8> Cached;
		if (FAmbarCache::Read(Job.DiskCategory, Job.DiskKey, Cached))
		{
			if (Store(Job.Key, Cached, Job.bNearest))
			{
				Queued.Remove(Job.Key);
				continue;
			}
		}

		++InFlight;
		const TWeakPtr<FAmbarImages> Weak = AsShared();
		const int32 Gen = Generation;
		FAmbarClient::FetchBytes(Job.Url, [Weak, Job, Gen](bool bOk, const FString&, const TArray<uint8>& Bytes)
		{
			if (const TSharedPtr<FAmbarImages> Self = Weak.Pin())
			{
				if (Gen == Self->Generation)
				{
					Self->Finish(Job, bOk, Bytes);
				}
			}
		});
	}
}

void FAmbarImages::Finish(const FPending& Job, bool bOk, const TArray<uint8>& Png)
{
	--InFlight;
	Queued.Remove(Job.Key);
	if (bOk && Store(Job.Key, Png, Job.bNearest))
	{
		FAmbarCache::Write(Job.DiskCategory, Job.DiskKey, Png);
	}
	else
	{
		Failed.Add(Job.Key, FPlatformTime::Seconds());
	}
	Pump();
}

bool FAmbarImages::Store(const FString& Key, const TArray<uint8>& Png, bool bNearest)
{
	if (Png.Num() == 0)
	{
		return false;
	}
	IImageWrapperModule& Module = FModuleManager::LoadModuleChecked<IImageWrapperModule>(TEXT("ImageWrapper"));
	const TSharedPtr<IImageWrapper> Wrapper = Module.CreateImageWrapper(EImageFormat::PNG);
	if (!Wrapper.IsValid() || !Wrapper->SetCompressed(Png.GetData(), Png.Num()))
	{
		return false;
	}
	TArray64<uint8> Raw;
	if (!Wrapper->GetRaw(ERGBFormat::BGRA, 8, Raw))
	{
		return false;
	}
	const int32 Width = Wrapper->GetWidth();
	const int32 Height = Wrapper->GetHeight();
	if (Width <= 0 || Height <= 0 || Raw.Num() != static_cast<int64>(Width) * Height * 4)
	{
		return false;
	}

	UTexture2D* Texture = UTexture2D::CreateTransient(Width, Height, PF_B8G8R8A8);
	if (!Texture || !Texture->GetPlatformData() || Texture->GetPlatformData()->Mips.Num() == 0)
	{
		return false;
	}
	void* Dest = Texture->GetPlatformData()->Mips[0].BulkData.Lock(LOCK_READ_WRITE);
	FMemory::Memcpy(Dest, Raw.GetData(), Raw.Num());
	Texture->GetPlatformData()->Mips[0].BulkData.Unlock();
	// Pixel art stays crisp when it is scaled up to fill a tile.
	Texture->Filter = bNearest ? TF_Nearest : TF_Bilinear;
	Texture->SRGB = true;
	Texture->UpdateResource();

	FEntry Entry;
	Entry.Texture = TStrongObjectPtr<UTexture2D>(Texture);
	Entry.Brush = MakeShared<FSlateBrush>();
	Entry.Brush->SetResourceObject(Texture);
	Entry.Brush->DrawAs = ESlateBrushDrawType::Image;
	// The brush's own vector type has changed between engine versions; build it from what it is.
	Entry.Brush->ImageSize = decltype(Entry.Brush->ImageSize)(static_cast<float>(Width), static_cast<float>(Height));
	Entry.LastUsed = ++Tick;
	Entries.Add(Key, MoveTemp(Entry));

	if (Entries.Num() > MaxEntries)
	{
		Evict();
	}
	return true;
}

void FAmbarImages::Evict()
{
	TArray<TPair<uint64, FString>> Order;
	Order.Reserve(Entries.Num());
	for (const TPair<FString, FEntry>& Pair : Entries)
	{
		Order.Emplace(Pair.Value.LastUsed, Pair.Key);
	}
	Order.Sort([](const TPair<uint64, FString>& A, const TPair<uint64, FString>& B) { return A.Key < B.Key; });
	for (int32 I = 0; I < Order.Num() / 4; ++I)
	{
		Entries.Remove(Order[I].Value);
	}
}
