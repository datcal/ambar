#pragma once

#include "CoreMinimal.h"
#include "AmbarImages.h"
#include "AmbarTypes.h"
#include "Widgets/SCompoundWidget.h"

class SVerticalBox;
class SWrapBox;

/** The right-hand panel: a large preview, the facts, tags, licence, other formats, and the import button. */
class SAmbarInspector : public SCompoundWidget
{
public:
	DECLARE_DELEGATE_OneParam(FOnImport, const FAmbarAsset&);
	DECLARE_DELEGATE_OneParam(FOnPickTag, const FString&);

	SLATE_BEGIN_ARGS(SAmbarInspector) {}
		SLATE_ARGUMENT(TSharedPtr<FAmbarImages>, Images)
		SLATE_EVENT(FOnImport, OnImport)
		SLATE_EVENT(FOnPickTag, OnPickTag)
		SLATE_ATTRIBUTE(bool, Busy)
	SLATE_END_ARGS()

	void Construct(const FArguments& InArgs);

	/** Shows one asset (fetching its detail), or the count when several are selected, or nothing. */
	void SetSelection(const TArray<TSharedPtr<FAmbarAsset>>& Selection);
	void SetImportedIds(const TSet<int64>* InImported) { Imported = InImported; }

private:
	void Rebuild();
	void LoadDetail(int64 AssetId);
	const FSlateBrush* GetPreview() const;
	FText ImportLabel() const;
	bool CanImport() const;
	FReply OnImportClicked();
	FReply OnOpenWeb();
	FReply OnCopyPath();
	TSharedRef<SWidget> Row(const FString& Label, const FString& Value) const;
	TSharedRef<SWidget> BuildTags();
	TSharedRef<SWidget> BuildVariants();

	TSharedPtr<FAmbarImages> Images;
	FOnImport OnImport;
	FOnPickTag OnPickTag;
	TAttribute<bool> Busy;
	const TSet<int64>* Imported = nullptr;

	TArray<TSharedPtr<FAmbarAsset>> Current;
	FAmbarDetail Detail;
	bool bHasDetail = false;
	FString DetailError;
	int32 Serial = 0;
	FString PreviewKey;

	TSharedPtr<SVerticalBox> Body;
};
