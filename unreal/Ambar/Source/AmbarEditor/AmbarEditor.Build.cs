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
			"InputCore",
			"Json",
			"Projects",
			"Slate",
			"SlateCore",
			"ToolMenus",
			"UnrealEd",
		});
	}
}
