using UnrealBuildTool;

public class AmbarEditor : ModuleRules
{
	public AmbarEditor(ReadOnlyTargetRules Target) : base(Target)
	{
		PCHUsage = PCHUsageMode.UseExplicitOrSharedPCHs;

		PublicDependencyModuleNames.AddRange(new string[]
		{
			"Core",
			"CoreUObject",
			"Engine",
			"DeveloperSettings",
		});

		PrivateDependencyModuleNames.AddRange(new string[]
		{
			"ApplicationCore",
			"AssetRegistry",
			"AssetTools",
			"ContentBrowser",
			"HTTP",
			"ImageWrapper",
			"EditorSubsystem",
			"InputCore",
			"Json",
			"LevelEditor",
			"Projects",
			"Slate",
			"SlateCore",
			"StatusBar",
			"ToolMenus",
			"ToolWidgets",
			"UnrealEd",
		});
	}
}
