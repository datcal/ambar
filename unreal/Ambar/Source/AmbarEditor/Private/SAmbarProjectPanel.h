#pragma once

#include "CoreMinimal.h"
#include "AmbarTypes.h"
#include "Widgets/SCompoundWidget.h"
#include "Widgets/Views/SListView.h"

/** One line of the "In this project" list: what the server says, merged with what the manifest says. */
struct FAmbarProjectRow
{
	FAmbarUse Use;
	FString Sha256;
	/** In the manifest but unknown to the server — imported while it was unreachable. */
	bool bUnsent = false;
};

/** Everything this project holds from the library: what is outdated, missing or never reported. */
class SAmbarProjectPanel : public SCompoundWidget
{
public:
	SLATE_BEGIN_ARGS(SAmbarProjectPanel) {}
		SLATE_EVENT(FSimpleDelegate, OnBack)
		/** Fired after something was re-imported or removed, so the grid can refresh its "in project" ticks. */
		SLATE_EVENT(FSimpleDelegate, OnChanged)
	SLATE_END_ARGS()

	void Construct(const FArguments& InArgs);

	/** Fetches the list again. */
	void Refresh();

private:
	TSharedRef<ITableRow> GenerateRow(TSharedPtr<FAmbarProjectRow> Row, const TSharedRef<STableViewBase>& Owner);
	void UpdateOne(const TSharedPtr<FAmbarProjectRow>& Row);
	void RemoveOne(const TSharedPtr<FAmbarProjectRow>& Row);
	void RecordOne(const TSharedPtr<FAmbarProjectRow>& Row);
	FReply UpdateAllOutdated();
	FReply WriteCredits();
	void SetStatus(const FString& Text, bool bError);

	FSimpleDelegate OnBack;
	FSimpleDelegate OnChanged;
	TArray<TSharedPtr<FAmbarProjectRow>> Rows;
	TSharedPtr<SListView<TSharedPtr<FAmbarProjectRow>>> List;
	FString Status;
	bool bStatusError = false;
	bool bBusy = false;
	int32 Serial = 0;
};
