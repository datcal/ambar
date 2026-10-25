#include "SAmbarProjectPanel.h"

#include "AmbarClient.h"
#include "AmbarImporter.h"
#include "AmbarNotify.h"
#include "AmbarProject.h"
#include "AmbarStyle.h"
#include "HAL/FileManager.h"
#include "Misc/FileHelper.h"
#include "Styling/AppStyle.h"
#include "Widgets/Input/SButton.h"
#include "Widgets/Layout/SBorder.h"
#include "Widgets/SBoxPanel.h"
#include "Widgets/Text/STextBlock.h"
#include "Widgets/Views/STableRow.h"

void SAmbarProjectPanel::Construct(const FArguments& InArgs)
{
	OnBack = InArgs._OnBack;
	OnChanged = InArgs._OnChanged;

	ChildSlot
	[
		SNew(SBorder).BorderImage(AmbarUI::PanelBrush()).Padding(FMargin(16.f))
		[
			SNew(SVerticalBox)
			+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(0.f, 0.f, 0.f, 12.f))
			[
				SNew(SHorizontalBox)
				+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(0.f, 0.f, 12.f, 0.f))
				[
					SNew(SButton).OnClicked_Lambda([this]() { OnBack.ExecuteIfBound(); return FReply::Handled(); })
					[
						SNew(STextBlock).Text(FText::FromString(TEXT("< Back to library")))
					]
				]
				+ SHorizontalBox::Slot().FillWidth(1.f).VAlign(VAlign_Center)
				[
					SNew(STextBlock).Text(FText::FromString(TEXT("In this project")))
					.Font(AmbarUI::Font(15, true)).ColorAndOpacity(AmbarUI::Text())
				]
				+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(4.f, 0.f))
				[
					SNew(SButton).OnClicked_Lambda([this]() { Refresh(); return FReply::Handled(); })
					[
						SNew(STextBlock).Text(FText::FromString(TEXT("Refresh")))
					]
				]
				+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(4.f, 0.f))
				[
					SNew(SButton).IsEnabled_Lambda([this]() { return !bBusy; })
					.OnClicked(this, &SAmbarProjectPanel::UpdateAllOutdated)
					.ToolTipText(FText::FromString(TEXT("Re-import everything the library has moved on from")))
					[
						SNew(STextBlock).Text(FText::FromString(TEXT("Update all outdated")))
					]
				]
				+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(4.f, 0.f))
				[
					SNew(SButton).ButtonStyle(FAppStyle::Get(), "PrimaryButton")
					.OnClicked(this, &SAmbarProjectPanel::WriteCredits)
					.ToolTipText(FText::FromString(TEXT("Write CREDITS.md next to the .uproject, built by the server from what this project uses")))
					[
						SNew(STextBlock).Text(FText::FromString(TEXT("Write CREDITS.md")))
					]
				]
			]
			+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(0.f, 0.f, 0.f, 8.f))
			[
				SNew(STextBlock)
				.Text_Lambda([this]() { return FText::FromString(Status); })
				.ColorAndOpacity_Lambda([this]() { return bStatusError ? AmbarUI::Bad() : AmbarUI::TextDim(); })
				.Font(AmbarUI::Font(9))
			]
			+ SVerticalBox::Slot().FillHeight(1.f)
			[
				SAssignNew(List, SListView<TSharedPtr<FAmbarProjectRow>>)
				.ListItemsSource(&Rows)
				.SelectionMode(ESelectionMode::None)
				.OnGenerateRow(this, &SAmbarProjectPanel::GenerateRow)
			]
		]
	];
}

void SAmbarProjectPanel::SetStatus(const FString& Text, bool bError)
{
	Status = Text;
	bStatusError = bError;
}

void SAmbarProjectPanel::Refresh()
{
	const int32 Mine = ++Serial;
	SetStatus(TEXT("Loading..."), false);
	const TWeakPtr<SAmbarProjectPanel> Weak = StaticCastSharedRef<SAmbarProjectPanel>(AsShared());

	FAmbarClient::ProjectUses(FAmbarProject::Uuid(),
		[Weak, Mine](bool bOk, const FString& Message, const TSharedPtr<FJsonObject>& Json)
		{
			const TSharedPtr<SAmbarProjectPanel> Self = Weak.Pin();
			if (!Self.IsValid() || Self->Serial != Mine)
			{
				return;
			}

			Self->Rows.Reset();
			TSet<int64> OnServer;
			if (bOk)
			{
				const TArray<TSharedPtr<FJsonValue>>* Uses = nullptr;
				if (Json->TryGetArrayField(TEXT("uses"), Uses) && Uses)
				{
					for (const TSharedPtr<FJsonValue>& V : *Uses)
					{
						const TSharedPtr<FJsonObject>* Obj = nullptr;
						if (V.IsValid() && V->TryGetObject(Obj) && Obj)
						{
							const TSharedPtr<FAmbarProjectRow> Row = MakeShared<FAmbarProjectRow>();
							Row->Use = FAmbarUse::FromJson(*Obj);
							OnServer.Add(Row->Use.AssetId);
							Self->Rows.Add(Row);
						}
					}
				}
			}

			// Imported while the server was unreachable: in the manifest, not on the server.
			for (const FAmbarProject::FEntry& E : FAmbarProject::Entries())
			{
				if (!OnServer.Contains(E.AssetId))
				{
					const TSharedPtr<FAmbarProjectRow> Row = MakeShared<FAmbarProjectRow>();
					Row->Use.AssetId = E.AssetId;
					Row->Use.ResPath = E.ResPath;
					Row->Use.Filename = E.Filename;
					Row->Use.Pack = E.Pack;
					Row->Sha256 = E.Sha256;
					Row->bUnsent = true;
					Self->Rows.Add(Row);
				}
			}

			int32 Outdated = 0;
			for (const TSharedPtr<FAmbarProjectRow>& R : Self->Rows)
			{
				Outdated += R->Use.bOutdated ? 1 : 0;
			}
			if (bOk)
			{
				Self->SetStatus(FString::Printf(TEXT("%d assets from the library, %d outdated."), Self->Rows.Num(), Outdated), false);
			}
			else
			{
				Self->SetStatus(Message + TEXT(" Showing what the project manifest knows."), true);
			}
			Self->List->RequestListRefresh();
		});
}

TSharedRef<ITableRow> SAmbarProjectPanel::GenerateRow(TSharedPtr<FAmbarProjectRow> Row, const TSharedRef<STableViewBase>& Owner)
{
	FString State = TEXT("Up to date");
	FLinearColor StateColor = AmbarUI::Good();
	if (Row->bUnsent)
	{
		State = TEXT("Server not told");
		StateColor = AmbarUI::Warn();
	}
	else if (Row->Use.bMissing)
	{
		State = TEXT("Gone from library");
		StateColor = AmbarUI::Bad();
	}
	else if (Row->Use.bOutdated)
	{
		State = TEXT("Outdated");
		StateColor = AmbarUI::Warn();
	}

	return SNew(STableRow<TSharedPtr<FAmbarProjectRow>>, Owner)
		.Padding(FMargin(0.f, 3.f))
		[
			SNew(SBorder).BorderImage(AmbarUI::CardBrush()).Padding(FMargin(12.f, 8.f))
			[
				SNew(SHorizontalBox)
				+ SHorizontalBox::Slot().FillWidth(1.f).VAlign(VAlign_Center)
				[
					SNew(SVerticalBox)
					+ SVerticalBox::Slot().AutoHeight()
					[
						SNew(STextBlock).Text(FText::FromString(Row->Use.Filename))
						.Font(AmbarUI::Font(10, true)).ColorAndOpacity(AmbarUI::Text())
					]
					+ SVerticalBox::Slot().AutoHeight()
					[
						SNew(STextBlock).Text(FText::FromString(Row->Use.Pack + TEXT("   ") + Row->Use.ResPath))
						.Font(AmbarUI::Font(8)).ColorAndOpacity(AmbarUI::TextDim())
					]
				]
				+ SHorizontalBox::Slot().AutoWidth().VAlign(VAlign_Center).Padding(FMargin(8.f, 0.f))
				[
					SNew(SBorder).BorderImage(AmbarUI::PillBrush()).Padding(FMargin(8.f, 2.f))
					[
						SNew(STextBlock).Text(FText::FromString(State)).Font(AmbarUI::Font(8, true))
						.ColorAndOpacity(StateColor)
					]
				]
				+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(2.f, 0.f))
				[
					SNew(SButton).Visibility(Row->bUnsent ? EVisibility::Visible : EVisibility::Collapsed)
					.IsEnabled_Lambda([this]() { return !bBusy; })
					.OnClicked_Lambda([this, Row]() { RecordOne(Row); return FReply::Handled(); })
					[
						SNew(STextBlock).Text(FText::FromString(TEXT("Tell server")))
					]
				]
				+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(2.f, 0.f))
				[
					SNew(SButton).Visibility(Row->Use.bMissing ? EVisibility::Collapsed : EVisibility::Visible)
					.IsEnabled_Lambda([this]() { return !bBusy; })
					.OnClicked_Lambda([this, Row]() { UpdateOne(Row); return FReply::Handled(); })
					.ToolTipText(FText::FromString(TEXT("Download this asset again and import it over the copy in the project")))
					[
						SNew(STextBlock).Text(FText::FromString(TEXT("Update")))
					]
				]
				+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(2.f, 0.f))
				[
					SNew(SButton).Visibility(Row->bUnsent ? EVisibility::Collapsed : EVisibility::Visible)
					.IsEnabled_Lambda([this]() { return !bBusy; })
					.OnClicked_Lambda([this, Row]() { RemoveOne(Row); return FReply::Handled(); })
					.ToolTipText(FText::FromString(TEXT("Stop counting this asset as used by the project. The imported files stay in Content.")))
					[
						SNew(STextBlock).Text(FText::FromString(TEXT("Forget")))
					]
				]
			]
		];
}

void SAmbarProjectPanel::UpdateOne(const TSharedPtr<FAmbarProjectRow>& Row)
{
	bBusy = true;
	SetStatus(FString::Printf(TEXT("Updating %s..."), *Row->Use.Filename), false);
	const TWeakPtr<SAmbarProjectPanel> Weak = StaticCastSharedRef<SAmbarProjectPanel>(AsShared());
	FAmbarClient::GetAsset(Row->Use.AssetId,
		[Weak](bool bOk, const FString& Message, const TSharedPtr<FJsonObject>& Json)
		{
			const TSharedPtr<SAmbarProjectPanel> Self = Weak.Pin();
			if (!Self.IsValid())
			{
				return;
			}
			if (!bOk)
			{
				Self->bBusy = false;
				Self->SetStatus(Message, true);
				return;
			}
			const FAmbarAsset Asset = FAmbarDetail::FromJson(Json).Asset;
			FAmbarImporter::Import(Asset, [Weak](const FAmbarImportResult& R)
			{
				AmbarUI::Notify(R.Message, R.bOk);
				if (const TSharedPtr<SAmbarProjectPanel> S = Weak.Pin())
				{
					S->bBusy = false;
					S->OnChanged.ExecuteIfBound();
					S->Refresh();
				}
			});
		});
}

void SAmbarProjectPanel::RemoveOne(const TSharedPtr<FAmbarProjectRow>& Row)
{
	const TWeakPtr<SAmbarProjectPanel> Weak = StaticCastSharedRef<SAmbarProjectPanel>(AsShared());
	const int64 AssetId = Row->Use.AssetId;
	FAmbarClient::RemoveUse(FAmbarProject::Uuid(), Row->Use.Id, [Weak, AssetId](bool bOk, const FString& Message)
	{
		const TSharedPtr<SAmbarProjectPanel> Self = Weak.Pin();
		if (!Self.IsValid())
		{
			return;
		}
		if (bOk)
		{
			FAmbarProject::Forget(AssetId);
			Self->OnChanged.ExecuteIfBound();
			Self->Refresh();
		}
		else
		{
			Self->SetStatus(Message, true);
		}
	});
}

void SAmbarProjectPanel::RecordOne(const TSharedPtr<FAmbarProjectRow>& Row)
{
	const TWeakPtr<SAmbarProjectPanel> Weak = StaticCastSharedRef<SAmbarProjectPanel>(AsShared());
	FAmbarAsset Asset;
	Asset.Id = Row->Use.AssetId;
	Asset.Sha256 = Row->Sha256;
	FAmbarClient::RecordUse(FAmbarProject::Uuid(), FAmbarProject::Name(), Asset, Row->Use.ResPath,
		[Weak](bool bOk, const FString& Message)
		{
			if (const TSharedPtr<SAmbarProjectPanel> Self = Weak.Pin())
			{
				if (bOk)
				{
					Self->Refresh();
				}
				else
				{
					Self->SetStatus(Message, true);
				}
			}
		});
}

FReply SAmbarProjectPanel::UpdateAllOutdated()
{
	TArray<TSharedPtr<FAmbarProjectRow>> Todo;
	for (const TSharedPtr<FAmbarProjectRow>& R : Rows)
	{
		if (R->Use.bOutdated && !R->Use.bMissing)
		{
			Todo.Add(R);
		}
	}
	if (Todo.Num() == 0)
	{
		SetStatus(TEXT("Nothing is outdated."), false);
		return FReply::Handled();
	}

	// One after another: each import is a synchronous editor operation, and the library is a NAS.
	const TWeakPtr<SAmbarProjectPanel> Weak = StaticCastSharedRef<SAmbarProjectPanel>(AsShared());
	const TSharedRef<TArray<TSharedPtr<FAmbarProjectRow>>> Queue = MakeShared<TArray<TSharedPtr<FAmbarProjectRow>>>(Todo);
	const TSharedRef<TFunction<void()>> Next = MakeShared<TFunction<void()>>();
	const TWeakPtr<TFunction<void()>> NextWeak = Next;
	bBusy = true;
	*Next = [Weak, Queue, NextWeak]()
	{
		const TSharedPtr<SAmbarProjectPanel> Self = Weak.Pin();
		if (!Self.IsValid())
		{
			return;
		}
		if (Queue->Num() == 0)
		{
			Self->bBusy = false;
			Self->OnChanged.ExecuteIfBound();
			Self->Refresh();
			return;
		}
		const TSharedPtr<FAmbarProjectRow> Row = (*Queue)[0];
		Queue->RemoveAt(0);
		Self->SetStatus(FString::Printf(TEXT("Updating %s (%d left)..."), *Row->Use.Filename, Queue->Num()), false);
		FAmbarClient::GetAsset(Row->Use.AssetId,
			[NextWeak](bool bOk, const FString&, const TSharedPtr<FJsonObject>& Json)
			{
				auto Continue = [NextWeak]()
				{
					if (const TSharedPtr<TFunction<void()>> N = NextWeak.Pin())
					{
						(*N)();
					}
				};
				if (!bOk)
				{
					Continue();
					return;
				}
				FAmbarImporter::Import(FAmbarDetail::FromJson(Json).Asset, [Continue](const FAmbarImportResult& R)
				{
					if (!R.bOk)
					{
						AmbarUI::Notify(R.Message, false);
					}
					Continue();
				});
			});
	};
	(*Next)();
	return FReply::Handled();
}

FReply SAmbarProjectPanel::WriteCredits()
{
	SetStatus(TEXT("Asking the server for the credits..."), false);
	const TWeakPtr<SAmbarProjectPanel> Weak = StaticCastSharedRef<SAmbarProjectPanel>(AsShared());
	FAmbarClient::Credits(FAmbarProject::Uuid(), [Weak](bool bOk, const FString& Message, const TArray<uint8>& Bytes)
	{
		const TSharedPtr<SAmbarProjectPanel> Self = Weak.Pin();
		if (!Self.IsValid())
		{
			return;
		}
		if (!bOk)
		{
			// An unknown project is a 404: nothing has been imported yet, so there is nothing to credit.
			Self->SetStatus(Message, true);
			return;
		}
		if (FFileHelper::SaveArrayToFile(Bytes, *FAmbarProject::CreditsPath()))
		{
			Self->SetStatus(FString::Printf(TEXT("Wrote %s"), *FAmbarProject::CreditsPath()), false);
			AmbarUI::Notify(TEXT("CREDITS.md written next to the .uproject"), true);
		}
		else
		{
			Self->SetStatus(TEXT("Could not write CREDITS.md."), true);
		}
	});
	return FReply::Handled();
}
