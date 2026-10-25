#include "SAmbarBrowser.h"

#include "AmbarCache.h"
#include "AmbarClient.h"
#include "AmbarImporter.h"
#include "AmbarNotify.h"
#include "AmbarProject.h"
#include "AmbarSettings.h"
#include "AmbarStyle.h"
#include "ContentBrowserModule.h"
#include "Framework/MultiBox/MultiBoxBuilder.h"
#include "HAL/FileManager.h"
#include "HAL/PlatformApplicationMisc.h"
#include "HAL/PlatformProcess.h"
#include "IContentBrowserSingleton.h"
#include "Misc/FileHelper.h"
#include "Misc/Paths.h"
#include "Misc/SecureHash.h"
#include "Modules/ModuleManager.h"
#include "SAmbarInspector.h"
#include "SAmbarProjectPanel.h"
#include "SAmbarTile.h"
#include "Styling/AppStyle.h"
#include "Widgets/Images/SImage.h"
#include "Widgets/Input/SButton.h"
#include "Widgets/Input/SCheckBox.h"
#include "Widgets/Input/SComboButton.h"
#include "Widgets/Input/SEditableTextBox.h"
#include "Widgets/Input/SHyperlink.h"
#include "Widgets/Input/SSearchBox.h"
#include "Widgets/Input/SSlider.h"
#include "Widgets/Input/STextComboBox.h"
#include "Widgets/Layout/SBorder.h"
#include "Widgets/Layout/SBox.h"
#include "Widgets/Layout/SScrollBox.h"
#include "Widgets/Layout/SSplitter.h"
#include "Widgets/Layout/SWidgetSwitcher.h"
#include "Widgets/SBoxPanel.h"
#include "Widgets/SNullWidget.h"
#include "Widgets/SOverlay.h"
#include "Widgets/Text/STextBlock.h"
#include "Widgets/Views/STableRow.h"

namespace
{
	struct FKindChip
	{
		const TCHAR* Label;
		const TCHAR* Value;
	};
	const FKindChip KindChips[] = {
		{TEXT("All"), TEXT("")},
		{TEXT("Images"), TEXT("image")},
		{TEXT("Sprites"), TEXT("spritesheet")},
		{TEXT("Textures"), TEXT("texture")},
		{TEXT("Models"), TEXT("model")},
		{TEXT("Audio"), TEXT("audio")},
		{TEXT("Fonts"), TEXT("font")},
	};

	FString FolderKey(const FString& Path)
	{
		return FString::Printf(TEXT("f-%s"), *FMD5::HashAnsiString(*Path));
	}

	int32 TileSizeFromSettings()
	{
		return FMath::Clamp(UAmbarSettings::Get()->TileSize, 64, 320);
	}
}

// ---------------------------------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------------------------------

void SAmbarBrowser::Construct(const FArguments& InArgs)
{
	const UAmbarSettings* Settings = UAmbarSettings::Get();
	Query.PageSize = FMath::Clamp(Settings->PageSize, 12, 200);
	Imported = FAmbarProject::ImportedIds();

	RootNode = MakeShared<FAmbarFolderNode>();
	RootNode->Name = TEXT("Library");
	RootNode->bHasChildren = true;
	RootItems.Add(RootNode);

	FormUrl = Settings->ServerUrl;
	FormToken = Settings->ApiToken;
	FormAccessId = Settings->AccessClientId;
	FormAccessSecret = Settings->AccessClientSecret;

	ChildSlot
	[
		SNew(SBorder)
		.BorderImage(AmbarUI::BackdropBrush())
		.Padding(FMargin(12.f))
		[
			SNew(SVerticalBox)
			+ SVerticalBox::Slot().AutoHeight()
			[
				BuildHeader()
			]
			+ SVerticalBox::Slot().FillHeight(1.f).Padding(FMargin(0.f, 10.f, 0.f, 0.f))
			[
				SAssignNew(Switcher, SWidgetSwitcher)
				+ SWidgetSwitcher::Slot()
				[
					BuildLibrary()
				]
				+ SWidgetSwitcher::Slot()
				[
					BuildSetup()
				]
				+ SWidgetSwitcher::Slot()
				[
					SAssignNew(ProjectPanel, SAmbarProjectPanel)
					.OnBack_Lambda([this]() { Switcher->SetActiveWidgetIndex(ViewLibrary); })
					.OnChanged_Lambda([this]() { RefreshImported(); })
				]
			]
			+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(0.f, 8.f, 0.f, 0.f))
			[
				BuildStatusBar()
			]
		]
	];

	Inspector->SetImportedIds(&Imported);

	if (Settings->IsConfigured())
	{
		Switcher->SetActiveWidgetIndex(ViewLibrary);
		Activate();
	}
	else
	{
		Switcher->SetActiveWidgetIndex(ViewSetup);
	}
}

TSharedRef<SWidget> SAmbarBrowser::BuildHeader()
{
	const TSharedRef<SHorizontalBox> Chips = SNew(SHorizontalBox);
	for (const FKindChip& Chip : KindChips)
	{
		const FString Value = Chip.Value;
		Chips->AddSlot().AutoWidth().Padding(FMargin(0.f, 0.f, 4.f, 0.f))
		[
			SNew(SCheckBox)
			.Style(&FAppStyle::Get().GetWidgetStyle<FCheckBoxStyle>("ToggleButtonCheckbox"))
			.IsChecked_Lambda([this, Value]() { return Query.Kind == Value ? ECheckBoxState::Checked : ECheckBoxState::Unchecked; })
			.OnCheckStateChanged_Lambda([this, Value](ECheckBoxState) { SetKind(Value); })
			[
				SNew(STextBlock).Text(FText::FromString(Chip.Label)).Font(AmbarUI::Font(9, true))
				.Margin(FMargin(8.f, 3.f))
			]
		];
	}

	return SNew(SBorder)
		.BorderImage(AmbarUI::PanelBrush())
		.Padding(FMargin(12.f, 10.f))
		[
			SNew(SHorizontalBox)
			+ SHorizontalBox::Slot().AutoWidth().VAlign(VAlign_Center).Padding(FMargin(0.f, 0.f, 16.f, 0.f))
			[
				SNew(STextBlock).Text(FText::FromString(TEXT("AMBAR")))
				.Font(AmbarUI::Font(16, true)).ColorAndOpacity(AmbarUI::Accent())
			]
			+ SHorizontalBox::Slot().FillWidth(1.f).VAlign(VAlign_Center).Padding(FMargin(0.f, 0.f, 12.f, 0.f))
			[
				SAssignNew(SearchBox, SSearchBox)
				.HintText(FText::FromString(TEXT("Search the library:  sword type:model  32x32  color:#8b3a3a")))
				.DelayChangeNotificationsWhileTyping(true)
				.OnTextChanged_Lambda([this](const FText& Text)
				{
					Query.Text = Text.ToString();
					Query.Page = 1;
					DoSearch();
				})
			]
			+ SHorizontalBox::Slot().AutoWidth().VAlign(VAlign_Center).Padding(FMargin(0.f, 0.f, 12.f, 0.f))
			[
				Chips
			]
			+ SHorizontalBox::Slot().AutoWidth().VAlign(VAlign_Center).Padding(FMargin(0.f, 0.f, 12.f, 0.f))
			[
				SNew(SBox).WidthOverride(170.f)
				[
					SAssignNew(SortCombo, STextComboBox)
					.OptionsSource(&SortLabels)
					.OnSelectionChanged(this, &SAmbarBrowser::OnSortChosen)
				]
			]
			+ SHorizontalBox::Slot().AutoWidth().VAlign(VAlign_Center).Padding(FMargin(0.f, 0.f, 12.f, 0.f))
			[
				SNew(SBox).WidthOverride(90.f)
				[
					SNew(SSlider)
					.ToolTipText(FText::FromString(TEXT("Thumbnail size")))
					.Value_Lambda([]() { return (TileSizeFromSettings() - 64) / 256.f; })
					.OnValueChanged_Lambda([this](float V)
					{
						UAmbarSettings::Get()->TileSize = 64 + FMath::RoundToInt(V * 256.f);
					})
					.OnMouseCaptureEnd_Lambda([]() { UAmbarSettings::Get()->SaveConfig(); })
				]
			]
			+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(2.f, 0.f))
			[
				SNew(SButton)
				.ToolTipText(FText::FromString(TEXT("Fetch the folders and results again")))
				.OnClicked_Lambda([this]() { ReloadEverything(); return FReply::Handled(); })
				[
					SNew(STextBlock).Text(FText::FromString(TEXT("Refresh")))
				]
			]
			+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(2.f, 0.f))
			[
				SNew(SButton)
				.ToolTipText(FText::FromString(TEXT("What this project already uses: updates, credits")))
				.OnClicked_Lambda([this]() { ProjectPanel->Refresh(); Switcher->SetActiveWidgetIndex(ViewProject); return FReply::Handled(); })
				[
					SNew(STextBlock).Text(FText::FromString(TEXT("In project")))
				]
			]
			+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(2.f, 0.f))
			[
				SNew(SComboButton)
				.ToolTipText(FText::FromString(TEXT("Local cache of folder names and pictures")))
				.OnGetMenuContent(this, &SAmbarBrowser::BuildCacheMenu)
				.ButtonContent()
				[
					SNew(STextBlock).Text(FText::FromString(TEXT("Cache")))
				]
			]
			+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(2.f, 0.f))
			[
				SNew(SButton)
				.ToolTipText(FText::FromString(TEXT("Server address and token")))
				.OnClicked_Lambda([this]() { Switcher->SetActiveWidgetIndex(ViewSetup); return FReply::Handled(); })
				[
					SNew(STextBlock).Text(FText::FromString(TEXT("Connection")))
				]
			]
		];
}

TSharedRef<SWidget> SAmbarBrowser::BuildLibrary()
{
	return SNew(SSplitter)
		.Style(FAppStyle::Get(), "DetailsView.Splitter")
		.PhysicalSplitterHandleSize(6.f)
		+ SSplitter::Slot().Value(0.2f).MinSize(160.f)
		[
			BuildFolderPane()
		]
		+ SSplitter::Slot().Value(0.56f)
		[
			BuildGridPane()
		]
		+ SSplitter::Slot().Value(0.24f).MinSize(240.f)
		[
			SAssignNew(Inspector, SAmbarInspector)
			.Images(Images)
			.Busy_Lambda([this]() { return bImporting; })
			.OnImport_Lambda([this](const FAmbarAsset& A) { QueueImport(A); })
			.OnPickTag_Lambda([this](const FString& Tag)
			{
				Query.Text = Tag;
				Query.Page = 1;
				SearchBox->SetText(FText::FromString(Tag));
				DoSearch();
			})
		];
}

TSharedRef<SWidget> SAmbarBrowser::BuildFolderPane()
{
	return SNew(SBorder)
		.BorderImage(AmbarUI::PanelBrush())
		.Padding(FMargin(6.f))
		[
			SAssignNew(FolderTree, STreeView<TSharedPtr<FAmbarFolderNode>>)
			.TreeItemsSource(&RootItems)
			.SelectionMode(ESelectionMode::Single)
			.OnGenerateRow(this, &SAmbarBrowser::GenerateFolderRow)
			.OnGetChildren_Lambda([this](TSharedPtr<FAmbarFolderNode> Node, TArray<TSharedPtr<FAmbarFolderNode>>& OutChildren)
			{
				EnsureChildren(Node);
				OutChildren = Node->Children;
			})
			.OnSelectionChanged_Lambda([this](TSharedPtr<FAmbarFolderNode> Node, ESelectInfo::Type Info)
			{
				if (Node.IsValid() && Info != ESelectInfo::Direct)
				{
					SelectFolder(Node->Path);
					FolderTree->SetItemExpansion(Node, true);
				}
			})
		];
}

TSharedRef<SWidget> SAmbarBrowser::BuildGridPane()
{
	return SNew(SVerticalBox)
		+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(4.f, 2.f, 4.f, 8.f))
		[
			SAssignNew(CrumbBox, SHorizontalBox)
		]
		+ SVerticalBox::Slot().FillHeight(1.f)
		[
			SNew(SBorder)
			.BorderImage(AmbarUI::PanelBrush())
			.Padding(FMargin(6.f))
			[
				SNew(SOverlay)
				+ SOverlay::Slot()
				[
					SAssignNew(TileView, STileView<TSharedPtr<FAmbarAsset>>)
					.ListItemsSource(&Items)
					.SelectionMode(ESelectionMode::Multi)
					.ItemWidth_Lambda([this]() { return TileEdge() + 24.f; })
					.ItemHeight_Lambda([this]() { return TileEdge() + 74.f; })
					.OnGenerateTile_Lambda([this](TSharedPtr<FAmbarAsset> Item, const TSharedRef<STableViewBase>& Owner) -> TSharedRef<ITableRow>
					{
						const int64 Id = Item->Id;
						return SNew(SAmbarTile, Owner)
							.Asset(Item)
							.Images(Images)
							.Edge_Lambda([this]() { return TileEdge(); })
							.InProject_Lambda([this, Id]() { return Imported.Contains(Id); })
							.Selected_Lambda([this, Item]() { return TileView.IsValid() && TileView->IsItemSelected(Item); });
					})
					.OnSelectionChanged(this, &SAmbarBrowser::OnSelectionChanged)
					.OnMouseButtonDoubleClick_Lambda([this](TSharedPtr<FAmbarAsset> Item) { QueueImport(*Item); })
					.OnContextMenuOpening(this, &SAmbarBrowser::BuildContextMenu)
				]
				+ SOverlay::Slot().HAlign(HAlign_Center).VAlign(VAlign_Center)
				[
					SNew(STextBlock)
					.Visibility_Lambda([this]() { return (Items.Num() == 0 && !bSearching) ? EVisibility::HitTestInvisible : EVisibility::Collapsed; })
					.Text_Lambda([this]()
					{
						return FText::FromString(ErrorText.IsEmpty() ? FString(TEXT("Nothing matches that.")) : ErrorText);
					})
					.ColorAndOpacity_Lambda([this]() { return ErrorText.IsEmpty() ? AmbarUI::TextDim() : AmbarUI::Bad(); })
					.Font(AmbarUI::Font(11))
					.Justification(ETextJustify::Center)
					.AutoWrapText(true)
				]
			]
		]
		+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(0.f, 8.f, 0.f, 0.f))
		[
			SAssignNew(PagerBox, SHorizontalBox)
		];
}

TSharedRef<SWidget> SAmbarBrowser::BuildStatusBar()
{
	return SNew(SHorizontalBox)
		+ SHorizontalBox::Slot().FillWidth(1.f).VAlign(VAlign_Center)
		[
			SNew(STextBlock)
			.Text_Lambda([this]() { return FText::FromString(StatusText); })
			.ColorAndOpacity_Lambda([this]() { return bStatusError ? AmbarUI::Bad() : AmbarUI::TextDim(); })
			.Font(AmbarUI::Font(9))
		]
		+ SHorizontalBox::Slot().AutoWidth().VAlign(VAlign_Center)
		[
			SNew(STextBlock)
			.Text_Lambda([]() { return FText::FromString(FAmbarClient::BaseUrl()); })
			.ColorAndOpacity(AmbarUI::TextDim())
			.Font(AmbarUI::Font(9))
		];
}

// ---------------------------------------------------------------------------------------------------
// Searching
// ---------------------------------------------------------------------------------------------------

float SAmbarBrowser::TileEdge() const
{
	return static_cast<float>(TileSizeFromSettings());
}

void SAmbarBrowser::SetStatus(const FString& Text, bool bError)
{
	StatusText = Text;
	bStatusError = bError;
}

void SAmbarBrowser::Activate()
{
	LoadSorts();
	EnsureChildren(RootNode);
	if (FolderTree.IsValid())
	{
		FolderTree->SetItemExpansion(RootNode, true);
		FolderTree->RequestTreeRefresh();
	}
	RebuildCrumbs();
	DoSearch();
}

void SAmbarBrowser::ReloadEverything()
{
	RootNode->Children.Reset();
	RootNode->bLoaded = false;
	RootNode->bLoading = false;
	Images->Reset();
	Activate();
}

void SAmbarBrowser::LoadSorts()
{
	const TWeakPtr<SAmbarBrowser> Weak = StaticCastSharedRef<SAmbarBrowser>(AsShared());
	FAmbarClient::Sorts([Weak](bool bOk, const FString&, const TSharedPtr<FJsonObject>& Json)
	{
		const TSharedPtr<SAmbarBrowser> Self = Weak.Pin();
		if (!Self.IsValid() || !bOk)
		{
			return;
		}
		Self->SortLabels.Reset();
		Self->SortValueByLabel.Reset();
		const FString Default = AmbarJson::Str(Json, TEXT("default"));
		TSharedPtr<FString> DefaultItem;

		const TArray<TSharedPtr<FJsonValue>>* Sorts = nullptr;
		if (Json->TryGetArrayField(TEXT("sorts"), Sorts) && Sorts)
		{
			for (const TSharedPtr<FJsonValue>& V : *Sorts)
			{
				const TSharedPtr<FJsonObject>* Obj = nullptr;
				if (!V.IsValid() || !V->TryGetObject(Obj) || !Obj)
				{
					continue;
				}
				const FString Label = AmbarJson::Str(*Obj, TEXT("label"));
				const FString Value = AmbarJson::Str(*Obj, TEXT("value"));
				const TSharedPtr<FString> Item = MakeShared<FString>(Label);
				Self->SortLabels.Add(Item);
				Self->SortValueByLabel.Add(Label, Value);
				if (Value == Default)
				{
					DefaultItem = Item;
				}
			}
		}
		if (Self->SortCombo.IsValid())
		{
			Self->SortCombo->RefreshOptions();
			if (DefaultItem.IsValid())
			{
				Self->SortCombo->SetSelectedItem(DefaultItem);
			}
		}
	});
}

void SAmbarBrowser::OnSortChosen(TSharedPtr<FString> Chosen, ESelectInfo::Type Info)
{
	if (!Chosen.IsValid() || Info == ESelectInfo::Direct)
	{
		return;
	}
	if (const FString* Value = SortValueByLabel.Find(*Chosen))
	{
		Query.Sort = *Value;
		Query.Page = 1;
		DoSearch();
	}
}

void SAmbarBrowser::SetKind(const FString& Kind)
{
	Query.Kind = Kind;
	Query.Page = 1;
	DoSearch();
}

void SAmbarBrowser::SelectFolder(const FString& Path)
{
	Query.Dir = Path;
	Query.Page = 1;
	RebuildCrumbs();
	DoSearch();
}

void SAmbarBrowser::GoToPage(int32 Page)
{
	Query.Page = FMath::Clamp(Page, 1, FMath::Max(1, Pages));
	DoSearch();
}

void SAmbarBrowser::DoSearch()
{
	const int32 Mine = ++SearchSerial;
	bSearching = true;
	ErrorText.Reset();
	SetStatus(TEXT("Searching..."));

	const TWeakPtr<SAmbarBrowser> Weak = StaticCastSharedRef<SAmbarBrowser>(AsShared());
	FAmbarClient::Search(Query, [Weak, Mine](bool bOk, const FString& Message, const TSharedPtr<FJsonObject>& Json)
	{
		const TSharedPtr<SAmbarBrowser> Self = Weak.Pin();
		if (!Self.IsValid() || Self->SearchSerial != Mine)
		{
			return; // a newer search is already running
		}
		Self->bSearching = false;
		Self->Items.Reset();

		if (!bOk)
		{
			Self->ErrorText = Message;
			Self->Total = 0;
			Self->Pages = 1;
			Self->SetStatus(Message, true);
		}
		else
		{
			const TArray<TSharedPtr<FJsonValue>>* Assets = nullptr;
			if (Json->TryGetArrayField(TEXT("assets"), Assets) && Assets)
			{
				for (const TSharedPtr<FJsonValue>& V : *Assets)
				{
					const TSharedPtr<FJsonObject>* Obj = nullptr;
					if (V.IsValid() && V->TryGetObject(Obj) && Obj)
					{
						Self->Items.Add(MakeShared<FAmbarAsset>(FAmbarAsset::FromJson(*Obj)));
					}
				}
			}
			Self->Total = static_cast<int64>(AmbarJson::Num(Json, TEXT("total")));
			Self->Pages = FMath::Max(1, static_cast<int32>(AmbarJson::Num(Json, TEXT("pages"))));
			Self->SetStatus(FString::Printf(TEXT("%lld assets"), Self->Total));
		}

		if (Self->TileView.IsValid())
		{
			Self->TileView->ClearSelection();
			Self->TileView->RequestListRefresh();
			Self->TileView->ScrollToTop();
		}
		Self->Inspector->SetSelection({});
		Self->RebuildPager();
	});
}

void SAmbarBrowser::RebuildPager()
{
	PagerBox->ClearChildren();
	PagerBox->AddSlot().FillWidth(1.f)[SNullWidget::NullWidget];

	auto AddButton = [this](const FString& Label, int32 Page, bool bCurrent, bool bEnabled)
	{
		PagerBox->AddSlot().AutoWidth().Padding(FMargin(2.f, 0.f))
		[
			SNew(SButton)
			.ButtonStyle(FAppStyle::Get(), "SimpleButton")
			.IsEnabled(bEnabled)
			.OnClicked_Lambda([this, Page]() { GoToPage(Page); return FReply::Handled(); })
			[
				SNew(STextBlock).Text(FText::FromString(Label))
				.Font(AmbarUI::Font(10, bCurrent))
				.ColorAndOpacity(bCurrent ? AmbarUI::Accent() : AmbarUI::Text())
			]
		];
	};

	const int32 Current = Query.Page;
	AddButton(TEXT("< Prev"), Current - 1, false, Current > 1);

	// 1 ... c-2 c-1 [c] c+1 c+2 ... N
	TArray<int32> Numbers;
	for (int32 P = 1; P <= Pages; ++P)
	{
		if (P == 1 || P == Pages || FMath::Abs(P - Current) <= 2)
		{
			Numbers.Add(P);
		}
	}
	int32 Previous = 0;
	for (const int32 P : Numbers)
	{
		if (P - Previous > 1)
		{
			PagerBox->AddSlot().AutoWidth().VAlign(VAlign_Center)
			[
				SNew(STextBlock).Text(FText::FromString(TEXT("..."))).ColorAndOpacity(AmbarUI::TextDim())
			];
		}
		AddButton(FString::FromInt(P), P, P == Current, true);
		Previous = P;
	}

	AddButton(TEXT("Next >"), Current + 1, false, Current < Pages);
	PagerBox->AddSlot().FillWidth(1.f)[SNullWidget::NullWidget];
}

void SAmbarBrowser::RebuildCrumbs()
{
	CrumbBox->ClearChildren();

	auto AddCrumb = [this](const FString& Label, const FString& Path, bool bLast)
	{
		CrumbBox->AddSlot().AutoWidth().VAlign(VAlign_Center)
		[
			SNew(SHyperlink)
			.Text(FText::FromString(Label))
			.OnNavigate_Lambda([this, Path]() { SelectFolder(Path); })
		];
		if (!bLast)
		{
			CrumbBox->AddSlot().AutoWidth().VAlign(VAlign_Center).Padding(FMargin(6.f, 0.f))
			[
				SNew(STextBlock).Text(FText::FromString(TEXT("/"))).ColorAndOpacity(AmbarUI::TextDim())
			];
		}
	};

	TArray<FString> Segments;
	Query.Dir.ParseIntoArray(Segments, TEXT("/"), true);
	AddCrumb(TEXT("Library"), FString(), Segments.Num() == 0);
	FString Path;
	for (int32 I = 0; I < Segments.Num(); ++I)
	{
		Path = Path.IsEmpty() ? Segments[I] : Path + TEXT("/") + Segments[I];
		AddCrumb(Segments[I], Path, I == Segments.Num() - 1);
	}
}

TArray<TSharedPtr<FAmbarAsset>> SAmbarBrowser::Selected() const
{
	return TileView.IsValid() ? TileView->GetSelectedItems() : TArray<TSharedPtr<FAmbarAsset>>();
}

void SAmbarBrowser::OnSelectionChanged(TSharedPtr<FAmbarAsset>, ESelectInfo::Type)
{
	Inspector->SetSelection(Selected());
}

// ---------------------------------------------------------------------------------------------------
// Folders
// ---------------------------------------------------------------------------------------------------

TSharedRef<ITableRow> SAmbarBrowser::GenerateFolderRow(TSharedPtr<FAmbarFolderNode> Node, const TSharedRef<STableViewBase>& Owner)
{
	const bool bRoot = Node->Path.IsEmpty();
	return SNew(STableRow<TSharedPtr<FAmbarFolderNode>>, Owner)
		.Padding(FMargin(2.f, 3.f))
		[
			SNew(SHorizontalBox)
			+ SHorizontalBox::Slot().AutoWidth().VAlign(VAlign_Center).Padding(FMargin(0.f, 0.f, 6.f, 0.f))
			[
				SNew(SImage)
				.Image(FAppStyle::GetBrush(bRoot ? "Icons.Package" : "Icons.FolderClosed"))
				.ColorAndOpacity(AmbarUI::Accent())
			]
			+ SHorizontalBox::Slot().FillWidth(1.f).VAlign(VAlign_Center)
			[
				SNew(STextBlock).Text(FText::FromString(Node->Name))
				.Font(AmbarUI::Font(10, bRoot)).ColorAndOpacity(AmbarUI::Text())
				.OverflowPolicy(ETextOverflowPolicy::Ellipsis)
			]
			+ SHorizontalBox::Slot().AutoWidth().VAlign(VAlign_Center).Padding(FMargin(6.f, 0.f, 4.f, 0.f))
			[
				SNew(STextBlock)
				.Text(Node->Assets > 0 ? FText::AsNumber(Node->Assets) : FText::GetEmpty())
				.Font(AmbarUI::Font(8)).ColorAndOpacity(AmbarUI::TextDim())
			]
		];
}

bool SAmbarBrowser::ApplyFolders(const TSharedPtr<FAmbarFolderNode>& Node, const TSharedPtr<FJsonObject>& Json)
{
	if (!Json.IsValid())
	{
		return false;
	}
	const TArray<TSharedPtr<FJsonValue>>* Folders = nullptr;
	if (!Json->TryGetArrayField(TEXT("folders"), Folders) || !Folders)
	{
		return false;
	}

	if (Node->Path.IsEmpty())
	{
		Node->Assets = static_cast<int32>(AmbarJson::Num(Json, TEXT("assets")));
	}

	// Merged by path, so nodes the tree already knows keep their expansion and their children.
	TArray<TSharedPtr<FAmbarFolderNode>> Merged;
	for (const TSharedPtr<FJsonValue>& V : *Folders)
	{
		const TSharedPtr<FJsonObject>* Obj = nullptr;
		if (!V.IsValid() || !V->TryGetObject(Obj) || !Obj)
		{
			continue;
		}
		const FString Path = AmbarJson::Str(*Obj, TEXT("path"));
		TSharedPtr<FAmbarFolderNode> Child;
		for (const TSharedPtr<FAmbarFolderNode>& Existing : Node->Children)
		{
			if (Existing->Path == Path)
			{
				Child = Existing;
				break;
			}
		}
		if (!Child.IsValid())
		{
			Child = MakeShared<FAmbarFolderNode>();
			Child->Path = Path;
		}
		Child->Name = AmbarJson::Str(*Obj, TEXT("name"));
		Child->Assets = static_cast<int32>(AmbarJson::Num(*Obj, TEXT("assets")));
		Child->bHasChildren = AmbarJson::Bool(*Obj, TEXT("has_children"));
		Merged.Add(Child);
	}
	Node->Children = MoveTemp(Merged);
	return true;
}

void SAmbarBrowser::EnsureChildren(const TSharedPtr<FAmbarFolderNode>& Node)
{
	if (!Node.IsValid() || !Node->bHasChildren || Node->bLoaded || Node->bLoading)
	{
		return;
	}
	Node->bLoading = true;

	// What was seen last time, immediately; the answer from the server replaces it a moment later.
	TArray<uint8> Cached;
	if (FAmbarCache::Read(TEXT("folders"), FolderKey(Node->Path), Cached))
	{
		FString Text;
		FFileHelper::BufferToString(Text, Cached.GetData(), Cached.Num());
		ApplyFolders(Node, AmbarJson::Parse(Text));
	}

	const TWeakPtr<SAmbarBrowser> Weak = StaticCastSharedRef<SAmbarBrowser>(AsShared());
	FAmbarClient::Folders(Node->Path, [Weak, Node](bool bOk, const FString&, const TSharedPtr<FJsonObject>& Json)
	{
		Node->bLoading = false;
		const TSharedPtr<SAmbarBrowser> Self = Weak.Pin();
		if (!bOk || !Self.IsValid())
		{
			return;
		}
		if (Self->ApplyFolders(Node, Json))
		{
			Node->bLoaded = true;
			const FTCHARToUTF8 Utf8(*AmbarJson::Write(Json.ToSharedRef()));
			FAmbarCache::Write(TEXT("folders"), FolderKey(Node->Path),
				TArray<uint8>(reinterpret_cast<const uint8*>(Utf8.Get()), Utf8.Length()));
		}
		if (Self->FolderTree.IsValid())
		{
			Self->FolderTree->RequestTreeRefresh();
		}
	});
}

// ---------------------------------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------------------------------

void SAmbarBrowser::RefreshImported()
{
	Imported = FAmbarProject::ImportedIds();
	if (TileView.IsValid())
	{
		TileView->RequestListRefresh();
	}
	if (Inspector.IsValid())
	{
		Inspector->SetSelection(Selected());
	}
}

void SAmbarBrowser::QueueImport(const FAmbarAsset& Asset)
{
	for (const FAmbarAsset& Queued : ImportQueue)
	{
		if (Queued.Id == Asset.Id)
		{
			return;
		}
	}
	ImportQueue.Add(Asset);
	if (!bImporting)
	{
		ImportedThisRun = 0;
		StartNextImport();
	}
}

void SAmbarBrowser::StartNextImport()
{
	if (ImportQueue.Num() == 0)
	{
		bImporting = false;
		if (ImportedThisRun > 0)
		{
			SetStatus(FString::Printf(TEXT("Imported %d asset%s."), ImportedThisRun, ImportedThisRun == 1 ? TEXT("") : TEXT("s")));
			if (UAmbarSettings::Get()->bSyncContentBrowser && !LastImportFolder.IsEmpty())
			{
				FContentBrowserModule& CB = FModuleManager::LoadModuleChecked<FContentBrowserModule>("ContentBrowser");
				CB.Get().SyncBrowserToFolders({LastImportFolder});
			}
		}
		RefreshImported();
		return;
	}

	bImporting = true;
	const FAmbarAsset Asset = ImportQueue[0];
	ImportQueue.RemoveAt(0);
	SetStatus(FString::Printf(TEXT("Importing %s ..."), *Asset.Filename));

	const TWeakPtr<SAmbarBrowser> Weak = StaticCastSharedRef<SAmbarBrowser>(AsShared());
	FAmbarImporter::Import(
		Asset,
		[Weak, Asset](const FAmbarImportResult& Result)
		{
			AmbarUI::Notify(Result.bOk ? Result.Message : FString::Printf(TEXT("%s: %s"), *Asset.Filename, *Result.Message), Result.bOk);
			if (const TSharedPtr<SAmbarBrowser> Self = Weak.Pin())
			{
				if (Result.bOk)
				{
					++Self->ImportedThisRun;
					Self->LastImportFolder = Result.Folder;
				}
				else
				{
					Self->SetStatus(Result.Message, true);
				}
				Self->StartNextImport();
			}
		},
		[Weak, Asset](float Fraction)
		{
			if (const TSharedPtr<SAmbarBrowser> Self = Weak.Pin())
			{
				Self->SetStatus(FString::Printf(TEXT("Downloading %s  %d%%"), *Asset.Filename, FMath::RoundToInt(Fraction * 100.f)));
			}
		});
}

TSharedPtr<SWidget> SAmbarBrowser::BuildContextMenu()
{
	const TArray<TSharedPtr<FAmbarAsset>> Sel = Selected();
	if (Sel.Num() == 0)
	{
		return nullptr;
	}

	FMenuBuilder Menu(/*bShouldCloseWindowAfterMenuSelection*/ true, nullptr);
	Menu.AddMenuEntry(
		FText::FromString(Sel.Num() == 1 ? TEXT("Import into project") : TEXT("Import selected")),
		FText::GetEmpty(), FSlateIcon(),
		FUIAction(FExecuteAction::CreateLambda([this, Sel]()
		{
			for (const TSharedPtr<FAmbarAsset>& A : Sel)
			{
				QueueImport(*A);
			}
		})));
	if (Sel.Num() == 1)
	{
		const TSharedPtr<FAmbarAsset> One = Sel[0];
		Menu.AddMenuEntry(
			FText::FromString(TEXT("Open in Ambar (browser)")), FText::GetEmpty(), FSlateIcon(),
			FUIAction(FExecuteAction::CreateLambda([One]()
			{
				FPlatformProcess::LaunchURL(*FAmbarClient::WebUrl(One->Id), nullptr, nullptr);
			})));
		Menu.AddMenuEntry(
			FText::FromString(TEXT("Copy library path")), FText::GetEmpty(), FSlateIcon(),
			FUIAction(FExecuteAction::CreateLambda([One]()
			{
				FPlatformApplicationMisc::ClipboardCopy(*One->Path);
			})));
		Menu.AddMenuEntry(
			FText::FromString(TEXT("Show this pack's folder")), FText::GetEmpty(), FSlateIcon(),
			FUIAction(FExecuteAction::CreateLambda([this, One]()
			{
				FString Dir = FPaths::GetPath(One->Path);
				SelectFolder(Dir);
			})));
	}
	return Menu.MakeWidget();
}

// ---------------------------------------------------------------------------------------------------
// Cache menu
// ---------------------------------------------------------------------------------------------------

TSharedRef<SWidget> SAmbarBrowser::BuildCacheMenu()
{
	int32 Files = 0;
	int64 Bytes = 0;
	FAmbarCache::Stats(Files, Bytes);

	FMenuBuilder Menu(true, nullptr);
	Menu.BeginSection("Info", FText::FromString(TEXT("Local cache")));
	Menu.AddMenuEntry(
		FText::FromString(FString::Printf(TEXT("%d files, %.1f MB  (limit %d MB)"), Files, Bytes / (1024.0 * 1024.0),
			UAmbarSettings::Get()->MaxCacheSizeMB)),
		FText::FromString(FAmbarCache::BaseRoot()), FSlateIcon(),
		FUIAction(FExecuteAction(), FCanExecuteAction::CreateLambda([]() { return false; })));
	Menu.EndSection();

	Menu.BeginSection("Settings", FText::FromString(TEXT("Keep on disk")));
	Menu.AddMenuEntry(
		FText::FromString(TEXT("Thumbnails and previews")),
		FText::FromString(TEXT("Keep pictures on disk so a folder you have seen opens instantly, even offline")),
		FSlateIcon(),
		FUIAction(
			FExecuteAction::CreateLambda([]()
			{
				UAmbarSettings* S = UAmbarSettings::Get();
				S->bCacheImages = !S->bCacheImages;
				S->SaveConfig();
			}),
			FCanExecuteAction(),
			FIsActionChecked::CreateLambda([]() { return UAmbarSettings::Get()->bCacheImages; })),
		NAME_None, EUserInterfaceActionType::ToggleButton);
	Menu.AddMenuEntry(
		FText::FromString(TEXT("Folder names")),
		FText::FromString(TEXT("Remember the folder tree; it is still refreshed from the server in the background")),
		FSlateIcon(),
		FUIAction(
			FExecuteAction::CreateLambda([]()
			{
				UAmbarSettings* S = UAmbarSettings::Get();
				S->bCacheFolders = !S->bCacheFolders;
				S->SaveConfig();
			}),
			FCanExecuteAction(),
			FIsActionChecked::CreateLambda([]() { return UAmbarSettings::Get()->bCacheFolders; })),
		NAME_None, EUserInterfaceActionType::ToggleButton);
	Menu.EndSection();

	Menu.BeginSection("Clear", FText::FromString(TEXT("Clear")));
	Menu.AddMenuEntry(
		FText::FromString(TEXT("Clear thumbnails and previews")), FText::GetEmpty(), FSlateIcon(),
		FUIAction(FExecuteAction::CreateLambda([this]()
		{
			FAmbarCache::ClearImages();
			Images->Reset();
			if (TileView.IsValid())
			{
				TileView->RebuildList();
			}
			SetStatus(TEXT("Cleared the picture cache."));
		})));
	Menu.AddMenuEntry(
		FText::FromString(TEXT("Clear folder names")), FText::GetEmpty(), FSlateIcon(),
		FUIAction(FExecuteAction::CreateLambda([this]()
		{
			FAmbarCache::ClearFolders();
			SetStatus(TEXT("Cleared the folder cache."));
		})));
	Menu.AddMenuEntry(
		FText::FromString(TEXT("Clear everything")), FText::GetEmpty(), FSlateIcon(),
		FUIAction(FExecuteAction::CreateLambda([this]()
		{
			FAmbarCache::ClearAll();
			Images->Reset();
			if (TileView.IsValid())
			{
				TileView->RebuildList();
			}
			SetStatus(TEXT("Cleared the whole cache."));
		})));
	Menu.AddMenuEntry(
		FText::FromString(TEXT("Open cache folder")), FText::GetEmpty(), FSlateIcon(),
		FUIAction(FExecuteAction::CreateLambda([]()
		{
			const FString Dir = FAmbarCache::BaseRoot();
			IFileManager::Get().MakeDirectory(*Dir, true);
			FPlatformProcess::ExploreFolder(*Dir);
		})));
	Menu.EndSection();

	return Menu.MakeWidget();
}

// ---------------------------------------------------------------------------------------------------
// Connection setup
// ---------------------------------------------------------------------------------------------------

TSharedRef<SWidget> SAmbarBrowser::BuildSetup()
{
	auto Field = [](const FString& Label, const FString& Hint, TFunction<FString()> Get, TFunction<void(const FString&)> Set, bool bPassword) -> TSharedRef<SWidget>
	{
		return SNew(SVerticalBox)
			+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(0.f, 10.f, 0.f, 3.f))
			[
				SNew(STextBlock).Text(FText::FromString(Label)).Font(AmbarUI::Font(9, true)).ColorAndOpacity(AmbarUI::TextDim())
			]
			+ SVerticalBox::Slot().AutoHeight()
			[
				SNew(SEditableTextBox)
				.HintText(FText::FromString(Hint))
				.IsPassword(bPassword)
				.Text_Lambda([Get]() { return FText::FromString(Get()); })
				.OnTextChanged_Lambda([Set](const FText& T) { Set(T.ToString()); })
			];
	};

	return SNew(SScrollBox)
		+ SScrollBox::Slot().HAlign(HAlign_Center)
		[
			SNew(SBox).WidthOverride(560.f)
			[
			SNew(SBorder).BorderImage(AmbarUI::PanelBrush()).Padding(FMargin(24.f))
			[
				SNew(SVerticalBox)
				+ SVerticalBox::Slot().AutoHeight()
				[
					SNew(STextBlock).Text(FText::FromString(TEXT("Connect to Ambar")))
					.Font(AmbarUI::Font(18, true)).ColorAndOpacity(AmbarUI::Text())
				]
				+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(0.f, 6.f, 0.f, 6.f))
				[
					SNew(STextBlock)
					.Text(FText::FromString(TEXT("Paste the address of your Ambar server and an API token (Ambar web UI > Settings > API tokens; tick write so imports are recorded). Everyone on the team uses their own token. Stored in your user settings, never in the project.")))
					.Font(AmbarUI::Font(9)).ColorAndOpacity(AmbarUI::TextDim()).AutoWrapText(true)
				]
				+ SVerticalBox::Slot().AutoHeight()
				[
					Field(TEXT("Server address"), TEXT("http://nas:8973  or  https://ambar.example.com"),
						[this]() { return FormUrl; }, [this](const FString& S) { FormUrl = S; }, false)
				]
				+ SVerticalBox::Slot().AutoHeight()
				[
					Field(TEXT("API token"), TEXT("ambar_..."),
						[this]() { return FormToken; }, [this](const FString& S) { FormToken = S; }, true)
				]
				+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(0.f, 16.f, 0.f, 0.f))
				[
					SNew(STextBlock).Text(FText::FromString(TEXT("Behind a Cloudflare tunnel with Cloudflare Access? Add the service token (optional).")))
					.Font(AmbarUI::Font(9)).ColorAndOpacity(AmbarUI::TextDim()).AutoWrapText(true)
				]
				+ SVerticalBox::Slot().AutoHeight()
				[
					Field(TEXT("Access client ID"), TEXT("xxxxxxxx.access"),
						[this]() { return FormAccessId; }, [this](const FString& S) { FormAccessId = S; }, false)
				]
				+ SVerticalBox::Slot().AutoHeight()
				[
					Field(TEXT("Access client secret"), TEXT(""),
						[this]() { return FormAccessSecret; }, [this](const FString& S) { FormAccessSecret = S; }, true)
				]
				+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(0.f, 18.f, 0.f, 0.f))
				[
					SNew(SHorizontalBox)
					+ SHorizontalBox::Slot().AutoWidth()
					[
						SNew(SButton)
						.ButtonStyle(FAppStyle::Get(), "PrimaryButton")
						.IsEnabled_Lambda([this]() { return !bTesting; })
						.OnClicked_Lambda([this]() { SaveAndTestConnection(); return FReply::Handled(); })
						[
							SNew(STextBlock).Text(FText::FromString(TEXT("Save and connect")))
						]
					]
					+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(8.f, 0.f, 0.f, 0.f))
					[
						SNew(SButton)
						.Visibility_Lambda([]() { return UAmbarSettings::Get()->IsConfigured() ? EVisibility::Visible : EVisibility::Collapsed; })
						.OnClicked_Lambda([this]() { Switcher->SetActiveWidgetIndex(ViewLibrary); return FReply::Handled(); })
						[
							SNew(STextBlock).Text(FText::FromString(TEXT("Back to library")))
						]
					]
				]
				+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(0.f, 12.f, 0.f, 0.f))
				[
					SNew(STextBlock)
					.Text_Lambda([this]() { return FText::FromString(SetupMessage); })
					.ColorAndOpacity_Lambda([this]() { return bSetupError ? AmbarUI::Bad() : AmbarUI::Good(); })
					.Font(AmbarUI::Font(9)).AutoWrapText(true)
				]
			]
			]
		];
}

void SAmbarBrowser::SaveAndTestConnection()
{
	UAmbarSettings* Settings = UAmbarSettings::Get();
	Settings->ServerUrl = FormUrl.TrimStartAndEnd();
	Settings->ApiToken = FormToken.TrimStartAndEnd();
	Settings->AccessClientId = FormAccessId.TrimStartAndEnd();
	Settings->AccessClientSecret = FormAccessSecret.TrimStartAndEnd();
	Settings->SaveConfig();
	Images->Reset();

	if (!Settings->IsConfigured())
	{
		SetupMessage = TEXT("Both the address and the token are needed.");
		bSetupError = true;
		return;
	}

	bTesting = true;
	bSetupError = false;
	SetupMessage = TEXT("Connecting...");
	const TWeakPtr<SAmbarBrowser> Weak = StaticCastSharedRef<SAmbarBrowser>(AsShared());
	FAmbarClient::Ping([Weak](bool bOk, const FString& Message, const TSharedPtr<FJsonObject>&)
	{
		const TSharedPtr<SAmbarBrowser> Self = Weak.Pin();
		if (!Self.IsValid())
		{
			return;
		}
		Self->bTesting = false;
		if (bOk)
		{
			Self->bSetupError = false;
			Self->SetupMessage = TEXT("Connected.");
			Self->Switcher->SetActiveWidgetIndex(ViewLibrary);
			Self->RootNode->Children.Reset();
			Self->RootNode->bLoaded = false;
			Self->RootNode->bLoading = false;
			Self->Activate();
		}
		else
		{
			Self->bSetupError = true;
			Self->SetupMessage = Message;
		}
	});
}
