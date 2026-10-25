#include "SAmbarTile.h"

#include "AmbarClient.h"
#include "AmbarStyle.h"
#include "Styling/AppStyle.h"
#include "Widgets/Images/SImage.h"
#include "Widgets/Layout/SBorder.h"
#include "Widgets/Layout/SBox.h"
#include "Widgets/Layout/SScaleBox.h"
#include "Widgets/SBoxPanel.h"
#include "Widgets/SOverlay.h"
#include "Widgets/Text/STextBlock.h"

void SAmbarTile::Construct(const FArguments& InArgs, const TSharedRef<STableViewBase>& OwnerTable)
{
	Asset = InArgs._Asset;
	Images = InArgs._Images;
	Edge = InArgs._Edge;
	Selected = InArgs._Selected;
	// The hash is in the key: a replaced file gets a new picture instead of a stale cached one.
	ImageKey = FString::Printf(TEXT("t%lld-%s"), Asset->Id, *Asset->Sha256.Left(8));

	if (Images.IsValid())
	{
		Images->Request(ImageKey, FAmbarClient::ThumbUrl(Asset->Id), TEXT("thumbs"), ImageKey, Asset->bPixelArt);
	}

	const TAttribute<bool> InProject = InArgs._InProject;

	STableRow<TSharedPtr<FAmbarAsset>>::Construct(
		STableRow<TSharedPtr<FAmbarAsset>>::FArguments()
			.Padding(FMargin(4.f))
			.ShowSelection(false)
			[
				SNew(SBorder)
				.Padding(FMargin(8.f))
				.BorderImage_Lambda([this]() -> const FSlateBrush*
				{
					if (Selected.Get(false))
					{
						return AmbarUI::CardSelectedBrush();
					}
					return IsHovered() ? AmbarUI::CardHoverBrush() : AmbarUI::CardBrush();
				})
				.ToolTipText(FText::FromString(Asset->Path))
				[
					SNew(SBox)
					.WidthOverride_Lambda([this]() { return FOptionalSize(Edge.Get(150.f)); })
					[
						SNew(SVerticalBox)
						+ SVerticalBox::Slot().AutoHeight()
						[
							SNew(SBox)
							.HeightOverride_Lambda([this]() { return FOptionalSize(Edge.Get(150.f)); })
							[
								SNew(SOverlay)
								+ SOverlay::Slot()
								[
									SNew(SBorder)
									.BorderImage(AmbarUI::ThumbWellBrush())
									.Padding(FMargin(4.f))
									[
										SNew(SScaleBox)
										.Stretch(EStretch::ScaleToFit)
										[
											SNew(SImage)
											.Image(this, &SAmbarTile::GetPicture)
										]
									]
								]
								+ SOverlay::Slot().HAlign(HAlign_Left).VAlign(VAlign_Top).Padding(FMargin(6.f))
								[
									SNew(SBorder)
									.BorderImage(AmbarUI::PillBrush())
									.Padding(FMargin(7.f, 2.f))
									[
										SNew(STextBlock)
										.Text(this, &SAmbarTile::GetBadge)
										.Font(AmbarUI::Font(8, true))
										.ColorAndOpacity(AmbarUI::Text())
									]
								]
								+ SOverlay::Slot().HAlign(HAlign_Right).VAlign(VAlign_Top).Padding(FMargin(6.f))
								[
									SNew(SBorder)
									.BorderImage(AmbarUI::PillBrush())
									.Padding(FMargin(6.f, 2.f))
									.Visibility_Lambda([InProject]() { return InProject.Get(false) ? EVisibility::Visible : EVisibility::Collapsed; })
									[
										SNew(STextBlock)
										.Text(FText::FromString(TEXT("In project")))
										.Font(AmbarUI::Font(8, true))
										.ColorAndOpacity(AmbarUI::Good())
									]
								]
							]
						]
						+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(2.f, 8.f, 2.f, 0.f))
						[
							SNew(STextBlock)
							.Text(FText::FromString(Asset->Filename))
							.Font(AmbarUI::Font(10, true))
							.ColorAndOpacity(AmbarUI::Text())
							.OverflowPolicy(ETextOverflowPolicy::Ellipsis)
						]
						+ SVerticalBox::Slot().AutoHeight().Padding(FMargin(2.f, 1.f, 2.f, 0.f))
						[
							SNew(STextBlock)
							.Text(FText::FromString(Asset->PackName))
							.Font(AmbarUI::Font(8))
							.ColorAndOpacity(AmbarUI::TextDim())
							.OverflowPolicy(ETextOverflowPolicy::Ellipsis)
						]
					]
				]
			],
		OwnerTable);
}

const FSlateBrush* SAmbarTile::GetPicture()
{
	if (Images.IsValid())
	{
		if (const FSlateBrush* Brush = Images->Find(ImageKey))
		{
			return Brush;
		}
		// Not there (failed, evicted, or still queued): ask again. Request is throttled and cheap.
		Images->Request(ImageKey, FAmbarClient::ThumbUrl(Asset->Id), TEXT("thumbs"), ImageKey, Asset->bPixelArt);
	}
	return FAppStyle::GetBrush("Checkerboard");
}

FText SAmbarTile::GetBadge() const
{
	FString Kind = Asset->Kind.ToUpper();
	if (Asset->VariantCount > 1)
	{
		Kind += FString::Printf(TEXT("  x%d"), Asset->VariantCount);
	}
	return FText::FromString(Kind);
}
