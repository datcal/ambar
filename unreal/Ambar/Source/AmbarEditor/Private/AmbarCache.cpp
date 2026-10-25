#include "AmbarCache.h"

#include "AmbarSettings.h"
#include "HAL/FileManager.h"
#include "HAL/PlatformProcess.h"
#include "Misc/DateTime.h"
#include "Misc/FileHelper.h"
#include "Misc/Paths.h"
#include "Misc/SecureHash.h"

namespace
{
	FString CategoryDir(const FString& Category)
	{
		return FPaths::Combine(FAmbarCache::Root(), Category);
	}

	FString SafeKey(const FString& Key)
	{
		FString Out;
		Out.Reserve(Key.Len());
		for (const TCHAR C : Key)
		{
			Out.AppendChar((FChar::IsAlnum(C) || C == TEXT('-') || C == TEXT('_') || C == TEXT('.')) ? C : TEXT('_'));
		}
		return Out;
	}

	FString PathFor(const FString& Category, const FString& Key)
	{
		return FPaths::Combine(CategoryDir(Category), SafeKey(Key) + TEXT(".bin"));
	}

	int32 WritesSinceTrim = 0;

	void DeleteTree(const FString& Dir)
	{
		IFileManager::Get().DeleteDirectory(*Dir, /*RequireExists*/ false, /*Tree*/ true);
	}

	bool CategoryEnabled(const FString& Category)
	{
		const UAmbarSettings* Settings = UAmbarSettings::Get();
		return Category == TEXT("folders") ? Settings->bCacheFolders : Settings->bCacheImages;
	}
}

FString FAmbarCache::BaseRoot()
{
	const UAmbarSettings* Settings = UAmbarSettings::Get();
	FString Base = Settings->CacheDirectoryOverride.TrimStartAndEnd();
	if (Base.IsEmpty())
	{
		Base = FPaths::Combine(FPlatformProcess::UserSettingsDir(), TEXT("AmbarUnreal"), TEXT("Cache"));
	}
	return FPaths::ConvertRelativePathToFull(Base);
}

FString FAmbarCache::Root()
{
	const FString Server = FMD5::HashAnsiString(*UAmbarSettings::Get()->BaseUrl().ToLower()).Left(10);
	return FPaths::Combine(BaseRoot(), Server);
}

bool FAmbarCache::Read(const FString& Category, const FString& Key, TArray<uint8>& OutBytes)
{
	if (!CategoryEnabled(Category))
	{
		return false;
	}
	const FString Path = PathFor(Category, Key);
	if (!FFileHelper::LoadFileToArray(OutBytes, *Path))
	{
		return false;
	}
	// Touch the file now and then, so eviction sees what is actually in use. Once an hour is enough
	// for a least-recently-used order and keeps a page of 60 tiles from being 60 metadata writes.
	const FDateTime Stamp = IFileManager::Get().GetTimeStamp(*Path);
	if ((FDateTime::UtcNow() - Stamp).GetTotalHours() > 1.0)
	{
		IFileManager::Get().SetTimeStamp(*Path, FDateTime::UtcNow());
	}
	return true;
}

void FAmbarCache::Write(const FString& Category, const FString& Key, const TArray<uint8>& Bytes)
{
	if (!CategoryEnabled(Category) || Bytes.Num() == 0)
	{
		return;
	}
	IFileManager::Get().MakeDirectory(*CategoryDir(Category), /*Tree*/ true);
	FFileHelper::SaveArrayToFile(Bytes, *PathFor(Category, Key));

	if (Category != TEXT("folders") && ++WritesSinceTrim >= 100)
	{
		WritesSinceTrim = 0;
		Trim();
	}
}

void FAmbarCache::Stats(int32& OutFiles, int64& OutBytes)
{
	OutFiles = 0;
	OutBytes = 0;
	TArray<FString> Files;
	IFileManager::Get().FindFilesRecursive(Files, *BaseRoot(), TEXT("*.bin"), /*Files*/ true, /*Dirs*/ false);
	for (const FString& File : Files)
	{
		++OutFiles;
		OutBytes += IFileManager::Get().FileSize(*File);
	}
}

void FAmbarCache::ClearImages()
{
	DeleteTree(CategoryDir(TEXT("thumbs")));
	DeleteTree(CategoryDir(TEXT("previews")));
}

void FAmbarCache::ClearFolders()
{
	DeleteTree(CategoryDir(TEXT("folders")));
}

void FAmbarCache::ClearAll()
{
	DeleteTree(BaseRoot());
}

void FAmbarCache::Trim()
{
	const int64 Limit = static_cast<int64>(FMath::Max(16, UAmbarSettings::Get()->MaxCacheSizeMB)) * 1024 * 1024;

	struct FEntry
	{
		FString Path;
		int64 Size;
		FDateTime Stamp;
	};
	TArray<FEntry> Entries;
	int64 Total = 0;
	TArray<FString> Files;
	IFileManager::Get().FindFilesRecursive(Files, *BaseRoot(), TEXT("*.bin"), /*Files*/ true, /*Dirs*/ false);
	for (FString File : Files)
	{
		FPaths::NormalizeFilename(File);
		// Folder names are tiny and are what makes browsing instant; only images are evicted.
		if (File.Contains(TEXT("/folders/")))
		{
			continue;
		}
		const int64 Size = IFileManager::Get().FileSize(*File);
		Entries.Add(FEntry{File, Size, IFileManager::Get().GetTimeStamp(*File)});
		Total += Size;
	}
	if (Total <= Limit)
	{
		return;
	}

	Entries.Sort([](const FEntry& A, const FEntry& B) { return A.Stamp < B.Stamp; });
	for (const FEntry& E : Entries)
	{
		if (Total <= Limit)
		{
			break;
		}
		if (IFileManager::Get().Delete(*E.Path, /*RequireExists*/ false, /*EvenReadOnly*/ true, /*Quiet*/ true))
		{
			Total -= E.Size;
		}
	}
}
