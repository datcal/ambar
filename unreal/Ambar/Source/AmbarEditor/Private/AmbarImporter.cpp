#include "AmbarImporter.h"

#include "AmbarClient.h"
#include "AmbarProject.h"
#include "AmbarSettings.h"
#include "AmbarSha256.h"
#include "AssetImportTask.h"
#include "AssetToolsModule.h"
#include "Engine/Texture2D.h"
#include "HAL/FileManager.h"
#include "IAssetTools.h"
#include "Misc/FileHelper.h"
#include "Misc/PackageName.h"
#include "Misc/Paths.h"
#include "UObject/UObjectGlobals.h"

namespace
{
	/** Package path segments: letters, digits, underscore and dash; everything else becomes underscore. */
	FString Segment(const FString& In, const TCHAR* Fallback)
	{
		FString Out;
		for (const TCHAR C : In)
		{
			Out.AppendChar((FChar::IsAlnum(C) || C == TEXT('_') || C == TEXT('-')) ? C : TEXT('_'));
		}
		return Out.IsEmpty() ? FString(Fallback) : Out;
	}

	void ApplyPixelArtDefaults(UTexture2D* Texture)
	{
		Texture->Modify();
		Texture->Filter = TF_Nearest;
		Texture->MipGenSettings = TMGS_NoMipmaps;
		Texture->CompressionSettings = TC_EditorIcon;
		Texture->LODGroup = TEXTUREGROUP_UI;
		Texture->PostEditChange();
		Texture->MarkPackageDirty();
	}
}

FString FAmbarImporter::DestinationFolder(const FAmbarAsset& Asset)
{
	FString Root = UAmbarSettings::Get()->ImportRoot.TrimStartAndEnd();
	if (!Root.StartsWith(TEXT("/")))
	{
		Root = TEXT("/Game/Ambar");
	}
	while (Root.EndsWith(TEXT("/")))
	{
		Root.LeftChopInline(1);
	}
	return FString::Printf(TEXT("%s/%s/%s"), *Root, *Segment(Asset.Kind, TEXT("other")),
		*Segment(Asset.PackSlug.IsEmpty() ? Asset.PackName : Asset.PackSlug, TEXT("pack")));
}

void FAmbarImporter::Import(const FAmbarAsset& Asset, TFunction<void(const FAmbarImportResult&)> Done,
	TFunction<void(float)> Progress)
{
	auto Fail = [Done](const FString& Message)
	{
		FAmbarImportResult R;
		R.Message = Message;
		Done(R);
	};
	if (!Asset.IsValid())
	{
		Fail(TEXT("That asset has no id."));
		return;
	}

	const uint64 Expected = static_cast<uint64>(FMath::Max<int64>(Asset.Size, 0));
	TFunction<void(uint64, uint64)> OnProgress;
	if (Progress && Expected > 0)
	{
		OnProgress = [Progress, Expected](uint64 Received, uint64)
		{
			Progress(FMath::Clamp(static_cast<float>(Received) / static_cast<float>(Expected), 0.f, 1.f));
		};
	}

	FAmbarClient::DownloadFile(
		Asset.Id,
		[Asset, Done, Fail](bool bOk, const FString& Message, const TArray<uint8>& Bytes)
		{
			if (!bOk)
			{
				Fail(Message);
				return;
			}

			// What arrived must be what was listed. Provenance is the point of the library, and
			// recording an import for bytes nobody verified is worse than failing.
			const FString Got = AmbarHash::Sha256Hex(Bytes.GetData(), Bytes.Num());
			if (!Asset.Sha256.IsEmpty() && !Got.Equals(Asset.Sha256, ESearchCase::IgnoreCase))
			{
				Fail(FString::Printf(TEXT("The server sent different content than it listed for %s (expected %s..., got %s...). Nothing was imported."),
					*Asset.Filename, *Asset.Sha256.Left(8), *Got.Left(8)));
				return;
			}

			// Staged under Saved/, never inside Content/: the editor's importer copies it from here.
			const FString Staging = FPaths::Combine(FPaths::ProjectSavedDir(), TEXT("Ambar"), TEXT("Staging"),
				FString::Printf(TEXT("%lld"), Asset.Id), FPaths::GetCleanFilename(Asset.Filename));
			IFileManager::Get().MakeDirectory(*FPaths::GetPath(Staging), /*Tree*/ true);
			if (!FFileHelper::SaveArrayToFile(Bytes, *Staging))
			{
				Fail(FString::Printf(TEXT("Could not write %s."), *Staging));
				return;
			}

			const FString Folder = FAmbarImporter::DestinationFolder(Asset);
			UAssetImportTask* Task = NewObject<UAssetImportTask>();
			Task->AddToRoot();
			Task->Filename = Staging;
			Task->DestinationPath = Folder;
			Task->bAutomated = true; // no dialogs: this runs from a button, not from a drag
			Task->bReplaceExisting = true;
			Task->bSave = true;

			FAssetToolsModule& AssetTools = FModuleManager::LoadModuleChecked<FAssetToolsModule>("AssetTools");
			AssetTools.Get().ImportAssetTasks({Task});

			TArray<FString> Imported = Task->ImportedObjectPaths;
			Task->RemoveFromRoot();
			IFileManager::Get().DeleteDirectory(*FPaths::GetPath(Staging), /*RequireExists*/ false, /*Tree*/ true);

			if (Imported.Num() == 0)
			{
				Fail(FString::Printf(TEXT("Unreal has no importer for .%s files, or the import was refused. See the Output Log. (glb/gltf, fbx, obj, png, jpg, tga, wav are the usual ones.)"),
					*Asset.Ext));
				return;
			}

			if (UAmbarSettings::Get()->bPixelArtDefaults && Asset.bPixelArt)
			{
				for (const FString& ObjectPath : Imported)
				{
					if (UTexture2D* Texture = LoadObject<UTexture2D>(nullptr, *ObjectPath))
					{
						ApplyPixelArtDefaults(Texture);
					}
				}
			}

			const FString PackagePath = FPackageName::ObjectPathToPackageName(Imported[0]);
			FAmbarProject::Record(Asset, PackagePath);

			// Telling the server is what builds CREDITS.md and what protects the asset from any
			// removal. Failing is survivable: the manifest above is committed and can be replayed.
			FAmbarClient::RecordUse(FAmbarProject::Uuid(), FAmbarProject::Name(), Asset, PackagePath,
				[Folder, PackagePath, Done](bool bSent, const FString& Why)
				{
					FAmbarImportResult R;
					R.bOk = true;
					R.Folder = Folder;
					R.Message = bSent ? FString::Printf(TEXT("Imported %s"), *PackagePath)
									  : FString::Printf(TEXT("Imported %s. The server was not told: %s"), *PackagePath, *Why);
					Done(R);
				});
		},
		OnProgress);
}
