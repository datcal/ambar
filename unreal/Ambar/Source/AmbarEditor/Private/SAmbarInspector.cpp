#include "SAmbarInspector.h"

#include "AmbarClient.h"
#include "AmbarStyle.h"
#include "HAL/PlatformApplicationMisc.h"
#include "HAL/PlatformProcess.h"
#include "Styling/AppStyle.h"
#include "Widgets/Images/SImage.h"
#include "Widgets/Input/SButton.h"
#include "Widgets/Layout/SBorder.h"
#include "Widgets/Layout/SBox.h"
#include "Widgets/Layout/SScaleBox.h"
#include "Widgets/Layout/SScrollBox.h"
#include "Widgets/Layout/SWrapBox.h"
#include "Widgets/SBoxPanel.h"
#include "Widgets/Text/STextBlock.h"

void SAmbarInspector::Construct(const FArguments& InArgs)
{
	Images = InArgs._Images;
	OnImport = InArgs._OnImport;
	OnPickTag = InArgs._OnPickTag;
	Busy = InArgs._Busy;

	ChildSlot
	[
		SNew(SBorder)
		.BorderImage(AmbarUI::PanelBrush())
		.Padding(FMargin(14.f))
		[
			SAssignNew(Body, SVerticalBox)
		]
	];
	Rebuild();
}

void SAmbarInspector::SetSelection(const TArray<TSharedPtr<FAmbarAsset>>& Selection)
{
	Current = Selection;
	Detail = FAmbarDetail();
	bHasDetail = false;
	DetailError.Reset();
	PreviewKey.Reset();
	++Serial;

	if (Current.Num() == 1)
	{
		const FAmbarAsset& A = *Current[0];
		PreviewKey = FString::Printf(TEXT("p%lld-%s"), A.Id, *A.Sha256.Left(8));
		if (Images.IsValid())
		{
			Images->Request(PreviewKey, FAmbarClient::PreviewUrl(A.Id), TEXT("previews"), PreviewKey, A.bPixelArt);
		}
		LoadDetail(A.Id);
	}
	Rebuild();
}

void SAmbarInspector::LoadDetail(int64 AssetId)
{
	const int32 Mine = Serial;
	const TWeakPtr<SAmbarInspector> Weak = StaticCastSharedRef<SAmbarInspector>(AsShared());
	FAmbarClient::GetAsset(AssetId, [Weak, Mine](bool bOk, const FString& Message, const TSharedPtr<FJsonObject>& Json)
	{
		const TSharedPtr<SAmbarInspector> Self = Weak.Pin();
		if (!Self.IsValid() || Self->Serial != Mine)
		{
			return; // the selection moved on
		}
		if (bOk)
		{
			Self->Detail = FAmbarDetail::FromJson(Json);
			Self->bHasDetail = true;
		}
		else
		{
			Self->DetailError = Message;
		}
		Self->Rebuild();
	});
}

const FSlateBrush* SAmbarInspector::GetPreview() const
{
	if (Images.IsValid() && !PreviewKey.IsEmpty())
	{
		if (const FSlateBrush* Brush = Images->Find(PreviewKey))
		{
			return Brush;
		}
		// Until the large preview arrives, the grid's thumbnail is already in memory.
		if (Current.Num() == 1)
		{
			const FString ThumbKey = FString::Printf(TEXT("t%lld-%s"), Current[0]->Id, *Current[0]->Sha256.Left(8));
			if (const FSlateBrush* Thumb = Images->Find(ThumbKey))
			{
				return Thumb;
			}
		}
	}
	return FAppStyle::GetBrush("Checkerboard");
}

TSharedRef<SWidget> SAmbarInspector::Row(const FString& Label, const FString& Value) const
{
	return SNew(SHorizontalBox)
		+ SHorizontalBox::Slot().AutoWidth().Padding(FMargin(0.f, 2.f, 10.f, 2.f))
		[
			SNew(SBox).WidthOverride(74.f)
			[
				SNew(STextBlock).Text(FText::FromString(Label)).Font(AmbarUI::Font(9)).ColorAndOpacity(AmbarUI::TextDim())
			]
		]
		+ SHorizontalBox::Slot().FillWidth(1.f).Padding(FMargin(0.f, 2.f))
		[
			SNew(STextBlock).Text(FText::FromString(Value)).Font(AmbarUI::Font(9)).ColorAndOpacity(AmbarUI::Text())
			.AutoWrapText(true)
		];
}

void SAmbarInspector::Rebuild()
{
	Body->ClearChildren();

	if (Current.Num() == 0)
	{
		Body->AddSlot().HAlign(HAlign_Center).VAlign(VAlign_Center)
		[
			SNew(STextBlock)
			.Text(FText::FromString(TEXT("Select an asset to see it here.\nDouble-click to import.")))
			.Justification(ETextJustify::Center)
			.Font(AmbarUI::Font(10))
			.ColorAndOpacity(AmbarUI::TextDim())
		];
		return;
	}

	if (Current.Num() > 1)
	{
		Body->AddSlot().AutoHeight().Padding(FMargin(0.f, 0.f, 0.f, 12.f))
		[
			SNew(STextBlock)
			.Text(FText::FromString(FString::Printf(TEXT("%d assets selected"), Current.Num())))
			.Font(AmbarUI::Font(14, true))
			.ColorAndOpacity(AmbarUI::Text())
		];
		Body->AddSlot().AutoHeight()
		[
			SNew(SButton)
			.HAlign(HAlign_Center)
			.ButtonStyle(FAppStyle::Get(), "PrimaryButton")
			.IsEnabled(this, &SAmbarInspector::CanImport)
			.OnClicked(this, &SAmbarInspector::OnImportClicked)
			[
				SNew(STextBlock).Text(this, &SAmbarInspector::ImportLabel)
			]
		];
		return;
	}

	const FAmbarAsset& A = bHasDetail && Detail.Asset.IsValid() ? Detail.Asset : *Current[0];

	Body->AddSlot().AutoHeight()
	[
		SNew(SBox).HeightOverride(240.f)
		[
			SNew(SBorder).BorderImage(AmbarUI::ThumbWellBrush()).Padding(FMargin(6.f))
			[
				SNew(SScaleBox).Stretch(EStretch::ScaleToFit)
				[
					SNew(SImage).Image(this, &SAmbarInspector::GetPreview)
				]
			]
		]
	];

	Body->AddSlot().AutoHeight().Padding(FMargin(0.f, 12.f, 0.f, 0.f))
	[
		SNew(STextBlock).Text(FText::FromString(A.Filename)).Font(AmbarUI::Font(13, true))
		.ColorAndOpacity(AmbarUI::Text()).AutoWrapText(true)
	];
	Body->AddSlot().AutoHeight().Padding(FMargin(0.f, 2.f, 0.f, 10.f))
	[
		SNew(STextBlock).Text(FText::FromString(A.PackName)).Font(AmbarUI::Font(9))
		.ColorAndOpacity(AmbarUI::Accent())
	];

	Body->AddSlot().AutoHeight().Padding(FMargin(0.f, 0.f, 0.f, 6.f))
	[
		SNew(SHorizontalBox)
		+ SHorizontalBox::Slot().FillWidth(1.f).Padding(FMargin(0.f, 0.f, 4.f, 0.f))
		[
			SNew(SButton)
			.HAlign(HAlign_Center)
			.ButtonStyle(FAppStyle::Get(), "PrimaryButton")
			.IsEnabled(this, &SAmbarInspector::CanImport)
			.OnClicked(this, &SAmbarInspector::OnImportClicked)
			[
				SNew(STextBlock).Text(this, &SAmbarInspector::ImportLabel)
			]
		]
		+ SHorizontalBox::Slot().AutoWidth()
		[
			SNew(SButton).OnClicked(this, &SAmbarInspector::OnOpenWeb)
			.ToolTipText(FText::FromString(TEXT("Open this asset in Ambar's web UI")))
			[
				SNew(STextBlock).Text(FText::FromString(TEXT("Open in browser")))
			]
		]
	];

	if (!DetailError.IsEmpty())
	{
		Body->AddSlot().AutoHeight().Padding(FMargin(0.f, 4.f))
		[
			SNew(STextBlock).Text(FText::FromString(DetailError)).ColorAndOpacity(AmbarUI::Bad())
			.Font(AmbarUI::Font(9)).AutoWrapText(true)
		];
	}

	const TSharedRef<SVerticalBox> Facts = SNew(SVerticalBox);
	Facts->AddSlot().AutoHeight()[Row(TEXT("Kind"), A.Kind)];
	Facts->AddSlot().AutoHeight()[Row(TEXT("Format"), A.Ext.ToUpper() + TEXT("  ") + A.SizeText())];
	if (A.Width > 0 && A.Height > 0)
	{
		Facts->AddSlot().AutoHeight()[Row(TEXT("Size"), FString::Printf(TEXT("%d x %d px"), A.Width, A.Height))];
	}
	if (A.FrameCount > 1)
	{
		Facts->AddSlot().AutoHeight()[Row(TEXT("Frames"), FString::FromInt(A.FrameCount))];
	}
	if (A.TriCount > 0)
	{
		Facts->AddSlot().AutoHeight()[Row(TEXT("Triangles"), FString::FromInt(A.TriCount))];
	}
	if (A.DurationMs > 0)
	{
		Facts->AddSlot().AutoHeight()[Row(TEXT("Length"), FString::Printf(TEXT("%.2f s"), A.DurationMs / 1000.0))];
	}
	Facts->AddSlot().AutoHeight()[Row(TEXT("Path"), A.Path)];
	if (bHasDetail)
	{
		Facts->AddSlot().AutoHeight()[Row(TEXT("Licence"), Detail.License.IsEmpty() ? TEXT("not recorded") : Detail.License)];
		if (!Detail.Author.IsEmpty())
		{
			Facts->AddSlot().AutoHeight()[Row(TEXT("Author"), Detail.Author)];
		}
	}
	const bool bIn = Imported && Imported->Contains(A.Id);
	Facts->AddSlot().AutoHeight()[Row(TEXT("Project"), bIn ? TEXT("already imported") : TEXT("not imported"))];

	Body->AddSlot().FillHeight(1.f)
	[
		SNew(SScrollBox)
		+ SScrollBox::Slot()
		[
			Facts
		]
		+ SScrollBox::Slot().Padding(FMargin(0.f, 10.f, 0.f, 0.f))
		[
			BuildTags()
		]
		+ SScrollBox::Slot().Padding(FMargin(0.f, 10.f, 0.f, 0.f))
		[
			BuildVariants()
		]
	];

	Body->AddSlot().AutoHeight().Padding(FMargin(0.f, 8.f, 0.f, 0.f))
	[
		SNew(SButton).HAlign(HAlign_Center).OnClicked(this, &SAmbarInspector::OnCopyPath)
		.ToolTipText(FText::FromString(TEXT("Copy the library path to the clipboard")))
		[
			SNew(STextBlock).Text(FText::FromString(TEXT("Copy library path"))).Font(AmbarUI::Font(9))
		]
	];
}

FText SAmbarInspector::ImportLabel() const
{
	if (Current.Num() > 1)
	{
		return FText::FromString(FString::Printf(TEXT("Import %d assets"), Current.Num()));
	}
	if (Current.Num() == 1 && Imported && Imported->Contains(Current[0]->Id))
	{
		return FText::FromString(TEXT("Import again"));
	}
	return FText::FromString(TEXT("Import into project"));
}

bool SAmbarInspector::CanImport() const
{
	return Current.Num() > 0 && !Busy.Get(false);
}

FReply SAmbarInspector::OnImportClicked()
{
	for (const TSharedPtr<FAmbarAsset>& A : Current)
	{
		OnImport.ExecuteIfBound(*A);
	}
	return FReply::Handled();
}

FReply SAmbarInspector::OnOpenWeb()
{
	if (Current.Num() == 1)
	{
		FPlatformProcess::LaunchURL(*FAmbarClient::WebUrl(Current[0]->Id), nullptr, nullptr);
	}
	return FReply::Handled();
}

FReply SAmbarInspector::OnCopyPath()
{
	if (Current.Num() == 1)
	{
		FPlatformApplicationMisc::ClipboardCopy(*Current[0]->Path);
	}
	return FReply::Handled();
}

TSharedRef<SWidget> SAmbarInspector::BuildTags()
{
	const TSharedRef<SWrapBox> Wrap = SNew(SWrapBox).UseAllottedSize(true);
	if (!bHasDetail)
	{
		return Wrap;
	}
	for (const FString& Tag : Detail.Tags)
	{
		Wrap->AddSlot().Padding(FMargin(0.f, 0.f, 4.f, 4.f))
		[
			SNew(SButton)
			.ButtonStyle(FAppStyle::Get(), "NoBorder")
			.ContentPadding(FMargin(0.f))
			.ToolTipText(FText::FromString(TEXT("Search for this tag")))
			.OnClicked_Lambda([this, Tag]()
			{
				OnPickTag.ExecuteIfBound(Tag);
				return FReply::Handled();
			})
			[
				SNew(SBorder).BorderImage(AmbarUI::ChipBrush()).Padding(FMargin(8.f, 2.f))
				[
					SNew(STextBlock).Text(FText::FromString(Tag)).Font(AmbarUI::Font(8))
					.ColorAndOpacity(AmbarUI::Text())
				]
			]
		];
	}
	return Wrap;
}

TSharedRef<SWidget> SAmbarInspector::BuildVariants()
{
	const TSharedRef<SVerticalBox> Box = SNew(SVerticalBox);
	if (!bHasDetail || Detail.Variants.Num() < 2)
	{
		return Box;
	}
	Box->AddSlot().AutoHeight().Padding(FMargin(0.f, 0.f, 0.f, 4.f))
	[
		SNew(STextBlock).Text(FText::FromString(TEXT("Other formats of this artwork")))
		.Font(AmbarUI::Font(9, true)).ColorAndOpacity(AmbarUI::TextDim())
	];
	for (const FAmbarAsset& V : Detail.Variants)
	{
		Box->AddSlot().AutoHeight().Padding(FMargin(0.f, 2.f))
		[
			SNew(SHorizontalBox)
			+ SHorizontalBox::Slot().FillWidth(1.f).VAlign(VAlign_Center)
			[
				SNew(STextBlock).Text(FText::FromString(V.Filename + TEXT("  ") + V.SizeText()))
				.Font(AmbarUI::Font(9)).ColorAndOpacity(AmbarUI::Text())
				.OverflowPolicy(ETextOverflowPolicy::Ellipsis)
			]
			+ SHorizontalBox::Slot().AutoWidth()
			[
				SNew(SButton)
				.IsEnabled_Lambda([this]() { return !Busy.Get(false); })
				.OnClicked_Lambda([this, V]()
				{
					OnImport.ExecuteIfBound(V);
					return FReply::Handled();
				})
				[
					SNew(STextBlock).Text(FText::FromString(TEXT("Import")))
				]
			]
		];
	}
	return Box;
}
