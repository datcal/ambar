#include "AmbarCache.h"
#include "AmbarSettings.h"
#include "Editor.h"
#include "Framework/Application/SlateApplication.h"
#include "Framework/Docking/LayoutExtender.h"
#include "Framework/Docking/TabManager.h"
#include "HAL/IConsoleManager.h"
#include "LevelEditor.h"
#include "Modules/ModuleManager.h"
#include "SAmbarBrowser.h"
#include "StatusBarSubsystem.h"
#include "Styling/AppStyle.h"
#include "ToolMenus.h"
#include "WidgetDrawerConfig.h"
#include "Widgets/Docking/SDockTab.h"

#define LOCTEXT_NAMESPACE "Ambar"

namespace
{
	const FName AmbarTabName(TEXT("AmbarBrowser"));
	const FName AmbarDrawerId(TEXT("AmbarDrawer"));
	// The level editor's own status bar, where the Content Drawer and the Output Log live.
	const FName LevelEditorStatusBar(TEXT("LevelEditor.StatusBar"));
	// A tab the default level editor layout keeps (closed) in its bottom area; Ambar is placed
	// beside it, so "Dock in layout" opens it there instead of in a floating window.
	const FName ContentBrowserTab(TEXT("ContentBrowserTab1"));
}

class FAmbarEditorModule : public IModuleInterface
{
public:
	virtual void StartupModule() override
	{
		FGlobalTabmanager::Get()
			->RegisterNomadTabSpawner(AmbarTabName, FOnSpawnTab::CreateRaw(this, &FAmbarEditorModule::SpawnTab))
			.SetDisplayName(LOCTEXT("TabTitle", "Ambar"))
			.SetTooltipText(LOCTEXT("TabTooltip", "Browse the Ambar asset library and import into this project"))
			.SetIcon(FSlateIcon(FAppStyle::GetAppStyleSetName(), "Icons.Package"))
			.SetMenuType(ETabSpawnerMenuType::Hidden);

		UToolMenus::RegisterStartupCallback(FSimpleMulticastDelegate::FDelegate::CreateRaw(this, &FAmbarEditorModule::RegisterMenus));

		// The drawer and the docked tab both belong to the level editor: its status bar, its layout.
		FLevelEditorModule& LevelEditor = FModuleManager::LoadModuleChecked<FLevelEditorModule>("LevelEditor");
		LayoutHandle = LevelEditor.OnRegisterLayoutExtensions().AddRaw(this, &FAmbarEditorModule::ExtendLayout);
		if (LevelEditor.GetLevelEditorInstance().IsValid())
		{
			RegisterDrawer();
		}
		else
		{
			LevelEditorHandle = LevelEditor.OnLevelEditorCreated().AddRaw(this, &FAmbarEditorModule::OnLevelEditorCreated);
		}

		// Fallbacks for layouts that hide the menus and the status bar.
		OpenCommand = MakeUnique<FAutoConsoleCommand>(
			TEXT("Ambar.Open"), TEXT("Open the Ambar asset browser as a tab"),
			FConsoleCommandDelegate::CreateLambda([]() { FGlobalTabmanager::Get()->TryInvokeTab(AmbarTabName); }));
		DrawerCommand = MakeUnique<FAutoConsoleCommand>(
			TEXT("Ambar.Drawer"), TEXT("Open or close the Ambar drawer above the status bar"),
			FConsoleCommandDelegate::CreateLambda([]()
			{
				if (GEditor)
				{
					if (UStatusBarSubsystem* StatusBar = GEditor->GetEditorSubsystem<UStatusBarSubsystem>())
					{
						StatusBar->TryToggleDrawer(AmbarDrawerId);
					}
				}
			}));

		// Keep the picture cache inside its limit, once per editor start.
		FAmbarCache::Trim();
	}

	virtual void ShutdownModule() override
	{
		UToolMenus::UnRegisterStartupCallback(this);
		UToolMenus::UnregisterOwner(this);
		OpenCommand.Reset();
		DrawerCommand.Reset();

		if (FLevelEditorModule* LevelEditor = FModuleManager::GetModulePtr<FLevelEditorModule>("LevelEditor"))
		{
			LevelEditor->OnRegisterLayoutExtensions().Remove(LayoutHandle);
			LevelEditor->OnLevelEditorCreated().Remove(LevelEditorHandle);
		}
		if (bDrawerRegistered && GEditor)
		{
			if (UStatusBarSubsystem* StatusBar = GEditor->GetEditorSubsystem<UStatusBarSubsystem>())
			{
				StatusBar->UnregisterDrawer(LevelEditorStatusBar, AmbarDrawerId);
			}
		}
		DrawerBrowser.Reset();

		if (FSlateApplication::IsInitialized())
		{
			FGlobalTabmanager::Get()->UnregisterNomadTabSpawner(AmbarTabName);
		}
	}

private:
	TSharedRef<SDockTab> SpawnTab(const FSpawnTabArgs&)
	{
		return SNew(SDockTab)
			.TabRole(ETabRole::NomadTab)
			.Label(LOCTEXT("TabLabel", "Ambar"))
			[
				SNew(SAmbarBrowser)
			];
	}

	/** Ambar, closed, beside the Content Browser's place in the level editor's bottom area. */
	void ExtendLayout(FLayoutExtender& Extender)
	{
		Extender.ExtendLayout(FTabId(ContentBrowserTab), ELayoutExtensionPosition::After,
			FTabManager::FTab(FTabId(AmbarTabName), ETabState::ClosedTab));
	}

	void OnLevelEditorCreated(TSharedPtr<ILevelEditor>)
	{
		RegisterDrawer();
	}

	/**
	 * A button on the status bar, beside the Content Drawer, that slides the library up over the
	 * viewport and puts it away again when it loses focus — the way the Content Drawer works, because
	 * that is how assets are picked while building a level. One browser serves every opening, so the
	 * folder, the search and the scroll position are where they were left.
	 */
	void RegisterDrawer()
	{
		if (bDrawerRegistered || !GEditor)
		{
			return;
		}
		UStatusBarSubsystem* StatusBar = GEditor->GetEditorSubsystem<UStatusBarSubsystem>();
		if (!StatusBar)
		{
			return;
		}

		FWidgetDrawerConfig Drawer(AmbarDrawerId);
		Drawer.GetDrawerContentDelegate.BindRaw(this, &FAmbarEditorModule::GetDrawerContent);
		Drawer.OnDrawerOpenedDelegate.BindLambda([](FName) {});
		Drawer.OnDrawerDismissedDelegate.BindLambda([](const TSharedPtr<SWidget>&) {});
		Drawer.ButtonText = LOCTEXT("DrawerButton", "Ambar");
		Drawer.ToolTipText = LOCTEXT("DrawerTooltip",
			"The Ambar asset library, above the status bar; it closes when it loses focus. \"Dock in layout\" in its header keeps it open as a tab. (Console: Ambar.Drawer)");
		Drawer.Icon = FAppStyle::Get().GetBrush("Icons.Package");
		StatusBar->RegisterDrawer(LevelEditorStatusBar, MoveTemp(Drawer));
		bDrawerRegistered = true;
	}

	TSharedRef<SWidget> GetDrawerContent()
	{
		if (!DrawerBrowser.IsValid())
		{
			DrawerBrowser = SNew(SAmbarBrowser)
				.InDrawer(true)
				.OnDockInLayout_Raw(this, &FAmbarEditorModule::DockInLayout);
		}
		return DrawerBrowser.ToSharedRef();
	}

	/** Closes the drawer and opens the tab, which the layout extension places in the bottom area. */
	void DockInLayout()
	{
		if (GEditor)
		{
			if (UStatusBarSubsystem* StatusBar = GEditor->GetEditorSubsystem<UStatusBarSubsystem>())
			{
				StatusBar->ForceDismissDrawer();
			}
		}
		FGlobalTabmanager::Get()->TryInvokeTab(AmbarTabName);
	}

	void RegisterMenus()
	{
		FToolMenuOwnerScoped OwnerScoped(this);

		const FUIAction Open(FExecuteAction::CreateLambda([]() { FGlobalTabmanager::Get()->TryInvokeTab(AmbarTabName); }));

		if (UToolMenu* Window = UToolMenus::Get()->ExtendMenu("LevelEditor.MainMenu.Window"))
		{
			FToolMenuSection& Section = Window->FindOrAddSection("WindowLayout");
			Section.AddMenuEntry(
				"OpenAmbar", LOCTEXT("MenuLabel", "Ambar Asset Library"),
				LOCTEXT("MenuTooltip", "Browse the Ambar asset library and import into this project"),
				FSlateIcon(FAppStyle::GetAppStyleSetName(), "Icons.Package"), Open);
		}

		if (UToolMenu* Toolbar = UToolMenus::Get()->ExtendMenu("LevelEditor.LevelEditorToolBar.User"))
		{
			FToolMenuSection& Section = Toolbar->FindOrAddSection("Ambar");
			Section.AddEntry(FToolMenuEntry::InitToolBarButton(
				"OpenAmbarToolbar", Open, LOCTEXT("ToolbarLabel", "Ambar"),
				LOCTEXT("ToolbarTooltip", "Open the Ambar asset library"),
				FSlateIcon(FAppStyle::GetAppStyleSetName(), "Icons.Package")));
		}
	}

	TUniquePtr<FAutoConsoleCommand> OpenCommand;
	TUniquePtr<FAutoConsoleCommand> DrawerCommand;
	TSharedPtr<SAmbarBrowser> DrawerBrowser;
	FDelegateHandle LayoutHandle;
	FDelegateHandle LevelEditorHandle;
	bool bDrawerRegistered = false;
};

IMPLEMENT_MODULE(FAmbarEditorModule, AmbarEditor)

#undef LOCTEXT_NAMESPACE
