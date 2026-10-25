#include "AmbarCache.h"
#include "AmbarSettings.h"
#include "HAL/IConsoleManager.h"
#include "Framework/Application/SlateApplication.h"
#include "Framework/Docking/TabManager.h"
#include "Modules/ModuleManager.h"
#include "SAmbarBrowser.h"
#include "Styling/AppStyle.h"
#include "ToolMenus.h"
#include "Widgets/Docking/SDockTab.h"

#define LOCTEXT_NAMESPACE "Ambar"

namespace
{
	const FName AmbarTabName(TEXT("AmbarBrowser"));
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

		// Fallback for layouts that hide the menus: type "Ambar.Open" in the console.
		OpenCommand = MakeUnique<FAutoConsoleCommand>(
			TEXT("Ambar.Open"), TEXT("Open the Ambar asset browser"),
			FConsoleCommandDelegate::CreateLambda([]() { FGlobalTabmanager::Get()->TryInvokeTab(AmbarTabName); }));

		// Keep the picture cache inside its limit, once per editor start.
		FAmbarCache::Trim();
	}

	virtual void ShutdownModule() override
	{
		UToolMenus::UnRegisterStartupCallback(this);
		UToolMenus::UnregisterOwner(this);
		OpenCommand.Reset();
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

		if (UToolMenu* Toolbar = UToolMenus::Get()->ExtendMenu("LevelEditor.LevelEditorToolBar.PlayToolBar"))
		{
			FToolMenuSection& Section = Toolbar->FindOrAddSection("Ambar");
			Section.AddEntry(FToolMenuEntry::InitToolBarButton(
				"OpenAmbarToolbar", Open, LOCTEXT("ToolbarLabel", "Ambar"),
				LOCTEXT("ToolbarTooltip", "Open the Ambar asset library"),
				FSlateIcon(FAppStyle::GetAppStyleSetName(), "Icons.Package")));
		}
	}

	TUniquePtr<FAutoConsoleCommand> OpenCommand;
};

IMPLEMENT_MODULE(FAmbarEditorModule, AmbarEditor)

#undef LOCTEXT_NAMESPACE
