#include "AmbarRequests.h"

#include "AmbarClient.h"
#include "AmbarImporter.h"
#include "AmbarNotify.h"
#include "AmbarSettings.h"
#include "AmbarTypes.h"
#include "Dom/JsonObject.h"
#include "Dom/JsonValue.h"
#include "Misc/App.h"

namespace
{
	/** How long the server holds a poll open when it has nothing to hand over. */
	constexpr int32 PollWaitSeconds = 25;

	/** One request's imports as they go. */
	struct FRun
	{
		int64 RequestId = 0;
		TArray<int64> AssetIds;
		FString Folder;
		int32 Next = 0;
		int32 Failed = 0;
		TArray<TSharedPtr<FJsonValue>> Items;

		void Add(int64 AssetId, bool bOk, const FString& Message, const FString& Path, const FString& InFolder)
		{
			const TSharedRef<FJsonObject> Item = MakeShared<FJsonObject>();
			Item->SetNumberField(TEXT("asset_id"), static_cast<double>(AssetId));
			Item->SetBoolField(TEXT("ok"), bOk);
			Item->SetStringField(TEXT("message"), Message);
			Item->SetStringField(TEXT("path"), Path);
			Item->SetStringField(TEXT("folder"), InFolder);
			Items.Add(MakeShared<FJsonValueObject>(Item));
			Failed += bOk ? 0 : 1;
		}
	};
}

void FAmbarRequests::Start()
{
	bRunning = true;
	// Not in the middle of the editor starting up.
	PollLater(5.f);
}

void FAmbarRequests::Stop()
{
	bRunning = false;
	if (Later.IsValid())
	{
		FTSTicker::GetCoreTicker().RemoveTicker(Later);
		Later.Reset();
	}
	if (InFlight.IsValid())
	{
		// Unbound before the cancel: the module is going away, and the callback's code is in it.
		InFlight->OnProcessRequestComplete().Unbind();
		InFlight->CancelRequest();
		InFlight.Reset();
	}
	Queue.Reset();
}

void FAmbarRequests::PollLater(float Seconds)
{
	if (!bRunning || Later.IsValid())
	{
		return;
	}
	const TWeakPtr<FAmbarRequests> Weak = AsShared();
	Later = FTSTicker::GetCoreTicker().AddTicker(FTickerDelegate::CreateLambda([Weak](float)
	{
		if (const TSharedPtr<FAmbarRequests> Self = Weak.Pin())
		{
			Self->Later.Reset();
			Self->Poll();
		}
		return false;
	}), Seconds);
}

void FAmbarRequests::Poll()
{
	if (!bRunning || bBusy || InFlight.IsValid())
	{
		return;
	}
	const UAmbarSettings* Settings = UAmbarSettings::Get();
	if (!Settings->bAcceptAssistantImports || !Settings->IsConfigured())
	{
		// The settings change without a restart, so look again now and then.
		PollLater(30.f);
		return;
	}

	const TWeakPtr<FAmbarRequests> Weak = AsShared();
	const double Started = FPlatformTime::Seconds();
	InFlight = FAmbarClient::TakeRequests(FApp::GetProjectName(), PollWaitSeconds,
		[Weak, Started](bool bOk, const FString& Why, const TSharedPtr<FJsonObject>& Json)
		{
			const TSharedPtr<FAmbarRequests> Self = Weak.Pin();
			if (!Self.IsValid() || !Self->bRunning)
			{
				return;
			}
			Self->InFlight.Reset();
			if (!bOk)
			{
				// Unreachable, a token without write, or a server older than this: none of them mends in a
				// second, and the browser already says what is wrong. An older server is asked rarely.
				Self->PollLater(Why.StartsWith(TEXT("Not found")) ? 600.f : 30.f);
				return;
			}

			const TArray<TSharedPtr<FJsonValue>>* Requests = nullptr;
			if (Json.IsValid() && Json->TryGetArrayField(TEXT("requests"), Requests))
			{
				for (const TSharedPtr<FJsonValue>& Value : *Requests)
				{
					const TSharedPtr<FJsonObject>* Obj = nullptr;
					if (!Value.IsValid() || !Value->TryGetObject(Obj))
					{
						continue;
					}
					FRequest R;
					R.Id = static_cast<int64>(AmbarJson::Num(*Obj, TEXT("id")));
					R.Folder = AmbarJson::Str(*Obj, TEXT("folder"));
					R.Via = AmbarJson::Str(*Obj, TEXT("via"));
					const TArray<TSharedPtr<FJsonValue>>* Ids = nullptr;
					if ((*Obj)->TryGetArrayField(TEXT("asset_ids"), Ids))
					{
						for (const TSharedPtr<FJsonValue>& Id : *Ids)
						{
							R.AssetIds.Add(static_cast<int64>(Id->AsNumber()));
						}
					}
					if (R.Id > 0 && R.AssetIds.Num() > 0)
					{
						Self->Queue.Add(MoveTemp(R));
					}
				}
			}

			if (Self->Queue.Num() > 0)
			{
				Self->RunNext();
			}
			else if (FPlatformTime::Seconds() - Started < 2.0)
			{
				// Nothing, at once: something between here and the server does not hold the poll open.
				Self->PollLater(10.f);
			}
			else
			{
				Self->Poll();
			}
		});
}

void FAmbarRequests::RunNext()
{
	if (!bRunning)
	{
		return;
	}
	if (Queue.Num() == 0)
	{
		bBusy = false;
		Poll();
		return;
	}
	const FRequest Next = Queue[0];
	Queue.RemoveAt(0);
	Run(Next);
}

void FAmbarRequests::Run(const FRequest& Request)
{
	bBusy = true;
	AmbarUI::Notify(FString::Printf(TEXT("Ambar: importing %d asset(s) for %s"), Request.AssetIds.Num(),
		Request.Via.IsEmpty() ? TEXT("an AI assistant") : *Request.Via), true);

	const TSharedRef<FRun> State = MakeShared<FRun>();
	State->RequestId = Request.Id;
	State->AssetIds = Request.AssetIds;
	State->Folder = Request.Folder;

	// One asset after another, as the browser's folder import does. The step holds only a weak pointer to
	// itself; whichever callback is in flight holds the strong one, so the chain lives exactly as long as it runs.
	const TWeakPtr<FAmbarRequests> Weak = AsShared();
	const TSharedRef<TFunction<void()>> Step = MakeShared<TFunction<void()>>();
	const TWeakPtr<TFunction<void()>> StepWeak = Step;
	*Step = [Weak, State, StepWeak]()
	{
		const TSharedPtr<FAmbarRequests> Self = Weak.Pin();
		const TSharedPtr<TFunction<void()>> Again = StepWeak.Pin();
		if (!Self.IsValid() || !Self->bRunning || !Again.IsValid())
		{
			return;
		}

		if (State->Next >= State->AssetIds.Num())
		{
			const TSharedRef<FJsonObject> Body = MakeShared<FJsonObject>();
			Body->SetStringField(TEXT("project"), FApp::GetProjectName());
			Body->SetArrayField(TEXT("items"), State->Items);
			const int32 Total = State->AssetIds.Num();
			const int32 Failed = State->Failed;
			FAmbarClient::ReportRequest(State->RequestId, AmbarJson::Write(Body), [Weak, Total, Failed](bool bSent, const FString& Why)
			{
				const TSharedPtr<FAmbarRequests> Self = Weak.Pin();
				if (!Self.IsValid() || !Self->bRunning)
				{
					return;
				}
				FString Message = Failed == 0
					? FString::Printf(TEXT("Ambar: imported %d asset(s) an AI assistant asked for."), Total)
					: FString::Printf(TEXT("Ambar: %d of the %d imports an AI assistant asked for failed."), Failed, Total);
				if (!bSent)
				{
					Message += TEXT(" The assistant was not told: ") + Why;
				}
				AmbarUI::Notify(Message, Failed == 0 && bSent);
				Self->bBusy = false;
				Self->RunNext();
			});
			return;
		}

		const int64 AssetId = State->AssetIds[State->Next++];
		FAmbarClient::GetAsset(AssetId, [State, AssetId, Again](bool bOk, const FString& Why, const TSharedPtr<FJsonObject>& Json)
		{
			if (!bOk)
			{
				State->Add(AssetId, false, Why, FString(), FString());
				(*Again)();
				return;
			}
			const FAmbarAsset Asset = FAmbarDetail::FromJson(Json).Asset;
			FAmbarImporter::Import(Asset, [State, AssetId, Again](const FAmbarImportResult& R)
			{
				State->Add(AssetId, R.bOk, R.Message, R.Path, R.Folder);
				(*Again)();
			}, nullptr, State->Folder);
		});
	};
	(*Step)();
}
