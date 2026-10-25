#pragma once

#include "CoreMinimal.h"
#include "AmbarImages.h"
#include "AmbarTypes.h"
#include "Widgets/Views/STableRow.h"

/** One thumbnail card in the grid: picture, kind badge, "in project" tick, name and pack. */
class SAmbarTile : public STableRow<TSharedPtr<FAmbarAsset>>
{
public:
	SLATE_BEGIN_ARGS(SAmbarTile) {}
		SLATE_ARGUMENT(TSharedPtr<FAmbarAsset>, Asset)
		SLATE_ARGUMENT(TSharedPtr<FAmbarImages>, Images)
		/** Edge length of the picture area, read live so the slider resizes tiles without rebuilding them. */
		SLATE_ATTRIBUTE(float, Edge)
		SLATE_ATTRIBUTE(bool, InProject)
	SLATE_END_ARGS()

	void Construct(const FArguments& InArgs, const TSharedRef<STableViewBase>& OwnerTable);

private:
	const FSlateBrush* GetPicture() const;
	FText GetBadge() const;

	TSharedPtr<FAmbarAsset> Asset;
	TSharedPtr<FAmbarImages> Images;
	TAttribute<float> Edge;
	FString ImageKey;
};
