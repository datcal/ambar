#pragma once

#include "CoreMinimal.h"
#include "AmbarImages.h"
#include "AmbarTypes.h"
#include "Widgets/SCompoundWidget.h"
#include "Widgets/Views/STileView.h"
#include "Widgets/Views/STreeView.h"

class SAmbarInspector;
class SAmbarProjectPanel;
class SEditableTextBox;
class SHorizontalBox;
class SSearchBox;
class STextComboBox;
class SWidgetSwitcher;

/** One folder of the library tree. Children are fetched the first time the node is opened. */
struct FAmbarFolderNode
{
	FString Name;
	FString Path;
	int32 Assets = 0;
	bool bHasChildren = false;
	/** True once the server has answered for this node (a cached answer does not count). */
	bool bLoaded = false;
	bool bLoading = false;
	TArray<TSharedPtr<FAmbarFolderNode>> Children;
};

/** The whole Ambar tab: folders on the left, results in the middle, the inspector on the right. */
class SAmbarBrowser : public SCompoundWidget
{
public:
	SLATE_BEGIN_ARGS(SAmbarBrowser) {}
	SLATE_END_ARGS()

	void Construct(const FArguments& InArgs);

private:
	enum EView
	{
		ViewLibrary = 0,
		ViewSetup = 1,
		ViewProject = 2,
	};

	// Layout
	TSharedRef<SWidget> BuildHeader();
	TSharedRef<SWidget> BuildLibrary();
	TSharedRef<SWidget> BuildFolderPane();
	TSharedRef<SWidget> BuildGridPane();
	TSharedRef<SWidget> BuildSetup();
	TSharedRef<SWidget> BuildStatusBar();
	TSharedRef<SWidget> BuildCacheMenu();
	TSharedPtr<SWidget> BuildContextMenu();

	// Library
	void Activate();
	void LoadSorts();
	void DoSearch();
	void SetKind(const FString& Kind);
	void SelectFolder(const FString& Path);
	void RebuildPager();
	void RebuildCrumbs();
	void GoToPage(int32 Page);
	void ReloadEverything();
	void SetStatus(const FString& Text, bool bError = false);
	void OnSortChosen(TSharedPtr<FString> Chosen, ESelectInfo::Type Info);
	void OnSelectionChanged(TSharedPtr<FAmbarAsset> Item, ESelectInfo::Type Info);
	TArray<TSharedPtr<FAmbarAsset>> Selected() const;
	float TileEdge() const;

	// Folders
	void EnsureChildren(const TSharedPtr<FAmbarFolderNode>& Node);
	bool ApplyFolders(const TSharedPtr<FAmbarFolderNode>& Node, const TSharedPtr<FJsonObject>& Json);
	TSharedRef<ITableRow> GenerateFolderRow(TSharedPtr<FAmbarFolderNode> Node, const TSharedRef<STableViewBase>& Owner);

	// Import
	void QueueImport(const FAmbarAsset& Asset);
	void StartNextImport();
	void RefreshImported();

	// Setup
	void SaveAndTestConnection();

	// State
	TSharedRef<FAmbarImages> Images = MakeShared<FAmbarImages>();
	FAmbarQuery Query;
	int32 SearchSerial = 0;
	int32 Pages = 1;
	int64 Total = 0;
	FString ErrorText;
	bool bSearching = false;
	TArray<TSharedPtr<FAmbarAsset>> Items;
	TSet<int64> Imported;

	TArray<FAmbarAsset> ImportQueue;
	bool bImporting = false;
	FString LastImportFolder;
	int32 ImportedThisRun = 0;

	FString StatusText;
	bool bStatusError = false;

	// Setup form
	FString FormUrl;
	FString FormToken;
	FString FormAccessId;
	FString FormAccessSecret;
	FString SetupMessage;
	bool bSetupError = false;
	bool bTesting = false;

	// Widgets
	TSharedPtr<SWidgetSwitcher> Switcher;
	TSharedPtr<SSearchBox> SearchBox;
	TSharedPtr<STextComboBox> SortCombo;
	TSharedPtr<STileView<TSharedPtr<FAmbarAsset>>> TileView;
	TSharedPtr<STreeView<TSharedPtr<FAmbarFolderNode>>> FolderTree;
	TSharedPtr<SAmbarInspector> Inspector;
	TSharedPtr<SAmbarProjectPanel> ProjectPanel;
	TSharedPtr<SHorizontalBox> PagerBox;
	TSharedPtr<SHorizontalBox> CrumbBox;

	TArray<TSharedPtr<FString>> SortLabels;
	TMap<FString, FString> SortValueByLabel;
	TSharedPtr<FAmbarFolderNode> RootNode;
	TArray<TSharedPtr<FAmbarFolderNode>> RootItems;
};
