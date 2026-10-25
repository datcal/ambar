#include "AmbarProject.h"

#include "HAL/FileManager.h"
#include "Misc/App.h"
#include "Misc/FileHelper.h"
#include "Misc/Guid.h"
#include "Misc/Paths.h"

namespace
{
	FString ProjectFile() { return FPaths::Combine(FAmbarProject::Dir(), TEXT("project.json")); }
	FString ManifestFile() { return FPaths::Combine(FAmbarProject::Dir(), TEXT("manifest.json")); }

	TSharedPtr<FJsonObject> ReadJson(const FString& Path)
	{
		FString Text;
		if (!FFileHelper::LoadFileToString(Text, *Path))
		{
			return nullptr;
		}
		return AmbarJson::Parse(Text);
	}

	void WriteJson(const FString& Path, const TSharedRef<FJsonObject>& Json)
	{
		IFileManager::Get().MakeDirectory(*FAmbarProject::Dir(), /*Tree*/ true);
		FFileHelper::SaveStringToFile(AmbarJson::Write(Json), *Path);
	}

	TSharedRef<FJsonObject> Manifest()
	{
		const TSharedPtr<FJsonObject> Existing = ReadJson(ManifestFile());
		return Existing.IsValid() ? Existing.ToSharedRef() : MakeShared<FJsonObject>();
	}
}

FString FAmbarProject::Dir()
{
	return FPaths::ConvertRelativePathToFull(FPaths::Combine(FPaths::ProjectDir(), TEXT(".ambar")));
}

FString FAmbarProject::Name()
{
	const TSharedPtr<FJsonObject> Json = ReadJson(ProjectFile());
	const FString Saved = AmbarJson::Str(Json, TEXT("name"));
	return Saved.IsEmpty() ? FString(FApp::GetProjectName()) : Saved;
}

FString FAmbarProject::Uuid()
{
	const TSharedPtr<FJsonObject> Json = ReadJson(ProjectFile());
	const FString Saved = AmbarJson::Str(Json, TEXT("uuid"));
	if (!Saved.IsEmpty())
	{
		return Saved;
	}
	const FString Fresh = FGuid::NewGuid().ToString(EGuidFormats::DigitsWithHyphensLower);
	const TSharedRef<FJsonObject> Out = MakeShared<FJsonObject>();
	Out->SetStringField(TEXT("uuid"), Fresh);
	Out->SetStringField(TEXT("name"), FApp::GetProjectName());
	WriteJson(ProjectFile(), Out);
	return Fresh;
}

TArray<FAmbarProject::FEntry> FAmbarProject::Entries()
{
	TArray<FEntry> Out;
	for (const TPair<FString, TSharedPtr<FJsonValue>>& Pair : Manifest()->Values)
	{
		const TSharedPtr<FJsonObject>* Obj = nullptr;
		if (!Pair.Value.IsValid() || !Pair.Value->TryGetObject(Obj) || !Obj)
		{
			continue;
		}
		FEntry E;
		E.AssetId = FCString::Atoi64(*Pair.Key);
		E.ResPath = AmbarJson::Str(*Obj, TEXT("res_path"));
		E.Sha256 = AmbarJson::Str(*Obj, TEXT("sha256"));
		E.Filename = AmbarJson::Str(*Obj, TEXT("filename"));
		E.Pack = AmbarJson::Str(*Obj, TEXT("pack"));
		Out.Add(MoveTemp(E));
	}
	return Out;
}

TSet<int64> FAmbarProject::ImportedIds()
{
	TSet<int64> Ids;
	for (const TPair<FString, TSharedPtr<FJsonValue>>& Pair : Manifest()->Values)
	{
		Ids.Add(FCString::Atoi64(*Pair.Key));
	}
	return Ids;
}

void FAmbarProject::Record(const FAmbarAsset& Asset, const FString& PackagePath)
{
	const TSharedRef<FJsonObject> Root = Manifest();
	const TSharedRef<FJsonObject> Entry = MakeShared<FJsonObject>();
	Entry->SetStringField(TEXT("res_path"), PackagePath);
	Entry->SetStringField(TEXT("sha256"), Asset.Sha256);
	Entry->SetStringField(TEXT("filename"), Asset.Filename);
	Entry->SetStringField(TEXT("pack"), Asset.PackName);
	Root->SetObjectField(FString::Printf(TEXT("%lld"), Asset.Id), Entry);
	WriteJson(ManifestFile(), Root);
}

void FAmbarProject::Forget(int64 AssetId)
{
	const TSharedRef<FJsonObject> Root = Manifest();
	Root->RemoveField(FString::Printf(TEXT("%lld"), AssetId));
	WriteJson(ManifestFile(), Root);
}

FString FAmbarProject::CreditsPath()
{
	return FPaths::ConvertRelativePathToFull(FPaths::Combine(FPaths::ProjectDir(), TEXT("CREDITS.md")));
}
