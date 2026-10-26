#include "AmbarImporter.h"

#include "AmbarClient.h"
#include "AmbarProject.h"
#include "AmbarSettings.h"
#include "AmbarSha256.h"
#include "AssetImportTask.h"
#include "AssetRegistry/AssetRegistryModule.h"
#include "AssetRegistry/IAssetRegistry.h"
#include "AssetToolsModule.h"
#include "Engine/Texture2D.h"
#include "HAL/FileManager.h"
#include "IAssetTools.h"
#include "FileHelpers.h"
#include "Misc/FileHelper.h"
#include "Modules/ModuleManager.h"
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

namespace
{
	FString ImportRoot()
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
		return Root;
	}

	/** The asset's folder in the library, each segment made safe for a package path. */
	FString MirroredDir(const FAmbarAsset& Asset)
	{
		TArray<FString> Parts;
		FPaths::GetPath(Asset.Path).ParseIntoArray(Parts, TEXT("/"), /*CullEmpty*/ true);
		FString Out;
		for (const FString& Part : Parts)
		{
			Out += TEXT("/") + Segment(Part, TEXT("_"));
		}
		return Out;
	}

	/** Engine-ready kinds go through the importers; Unreal's own packages are copied as they are. */
	bool IsUnrealPackage(const FAmbarAsset& Asset)
	{
		return Asset.Ext.Equals(TEXT("uasset"), ESearchCase::IgnoreCase)
			|| Asset.Ext.Equals(TEXT("umap"), ESearchCase::IgnoreCase);
	}

	void Finish(const FAmbarAsset& Asset, const FString& Recorded, const FString& Folder, const FString& Verb,
		TFunction<void(const FAmbarImportResult&)> Done)
	{
		FAmbarProject::Record(Asset, Recorded);
		// Telling the server is what builds CREDITS.md and what protects the asset from any
		// removal. Failing is survivable: the manifest above is committed and can be replayed.
		FAmbarClient::RecordUse(FAmbarProject::Uuid(), FAmbarProject::Name(), Asset, Recorded,
			[Folder, Recorded, Verb, Done](bool bSent, const FString& Why)
			{
				FAmbarImportResult R;
				R.bOk = true;
				R.Folder = Folder;
				R.Path = Recorded;
				R.Message = bSent ? FString::Printf(TEXT("%s %s"), *Verb, *Recorded)
								  : FString::Printf(TEXT("%s %s. The server was not told: %s"), *Verb, *Recorded, *Why);
				Done(R);
			});
	}

	/**
	 * A .uasset or .umap is a package that names its dependencies by package path (/Game/Textures/T_Hull),
	 * so it only works where it was saved. A pack shipped for Unreal keeps a Content folder for exactly
	 * that reason; the file goes to the same place under this project's Content — what the editor's own
	 * Migrate does. Without a Content folder in its library path there is no right place, so it goes
	 * under the import root and the message says its references may not resolve.
	 */
	void CopyIntoContent(const FAmbarAsset& Asset, const TArray<uint8>& Bytes, TFunction<void(const FAmbarImportResult&)> Done)
	{
		auto Fail = [Done](const FString& Message)
		{
			FAmbarImportResult R;
			R.Message = Message;
			Done(R);
		};

		FString Relative;
		bool bInPlace = false;
		const int32 At = Asset.Path.Find(TEXT("/Content/"), ESearchCase::CaseSensitive, ESearchDir::FromEnd);
		if (At != INDEX_NONE)
		{
			Relative = Asset.Path.Mid(At + 9);
			bInPlace = true;
		}
		else
		{
			FString Root = ImportRoot();
			Root.RemoveFromStart(TEXT("/Game"));
			Relative = (Root + MirroredDir(Asset)).TrimChar(TEXT('/')) + TEXT("/") + FPaths::GetCleanFilename(Asset.Path);
		}
		const FString Target = FPaths::ConvertRelativePathToFull(FPaths::Combine(FPaths::ProjectContentDir(), Relative));

		// Never over somebody's own work: a file already there is replaced only when it is this same
		// asset from an earlier import, or when it is byte for byte what would be written.
		if (IFileManager::Get().FileExists(*Target) && !FAmbarProject::ImportedIds().Contains(Asset.Id))
		{
			TArray<uint8> Existing;
			FFileHelper::LoadFileToArray(Existing, *Target);
			if (Existing != Bytes)
			{
				Fail(FString::Printf(TEXT("%s is already in this project and was not imported from Ambar; it was left as it is."),
					*Relative));
				return;
			}
		}

		IFileManager::Get().MakeDirectory(*FPaths::GetPath(Target), /*Tree*/ true);
		if (!FFileHelper::SaveArrayToFile(Bytes, *Target))
		{
			Fail(FString::Printf(TEXT("Could not write %s."), *Target));
			return;
		}

		// Tell the asset registry, so the Content Browser shows it without a restart.
		IAssetRegistry& Registry = FModuleManager::LoadModuleChecked<FAssetRegistryModule>("AssetRegistry").Get();
		Registry.ScanFilesSynchronous({Target}, /*bForceRescan*/ true);

		FString PackageName;
		if (!FPackageName::TryConvertFilenameToLongPackageName(Target, PackageName))
		{
			PackageName = TEXT("/Game/") + FPaths::ChangeExtension(Relative, TEXT(""));
		}
		const FString Folder = FPackageName::GetLongPackagePath(PackageName);
		Finish(Asset, PackageName, Folder,
			bInPlace ? TEXT("Copied") : TEXT("Copied (outside a Content folder in the library, so its references may not resolve)"),
			Done);
	}

	/**
	 * What Unreal has no importer for — source code, configs, notes — is still part of what was asked for,
	 * so it is copied as a plain file under <Project>/Ambar/, mirroring the library. Outside Source/ on
	 * purpose: code dropped into a module would be compiled on the next build, which is a decision for a
	 * person, not for an import.
	 */
	void CopyAsPlainFile(const FAmbarAsset& Asset, const TArray<uint8>& Bytes, TFunction<void(const FAmbarImportResult&)> Done)
	{
		const FString Relative = FString(TEXT("Ambar/")) + Asset.Path;
		const FString Target = FPaths::ConvertRelativePathToFull(FPaths::Combine(FPaths::ProjectDir(), Relative));
		IFileManager::Get().MakeDirectory(*FPaths::GetPath(Target), /*Tree*/ true);
		if (!FFileHelper::SaveArrayToFile(Bytes, *Target))
		{
			FAmbarImportResult R;
			R.Message = FString::Printf(TEXT("Could not write %s."), *Target);
			Done(R);
			return;
		}
		Finish(Asset, Relative, FString(), TEXT("Copied"), Done);
	}
}

EAmbarImportMode FAmbarImporter::ModeFor(const FAmbarAsset& Asset)
{
	if (IsUnrealPackage(Asset))
	{
		return EAmbarImportMode::CopyToContent;
	}
	static const TSet<FString> ImportKinds = {
		TEXT("image"), TEXT("spritesheet"), TEXT("texture"), TEXT("model"), TEXT("audio"),
		TEXT("font"), TEXT("hdri"), TEXT("video"),
	};
	return ImportKinds.Contains(Asset.Kind) ? EAmbarImportMode::Import : EAmbarImportMode::PlainFile;
}

FString FAmbarImporter::DestinationFolder(const FAmbarAsset& Asset)
{
	return ImportRoot() + MirroredDir(Asset);
}

void FAmbarImporter::Import(const FAmbarAsset& Asset, TFunction<void(const FAmbarImportResult&)> Done,
	TFunction<void(float)> Progress, const FString& FolderOverride)
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
		[Asset, Done, Fail, FolderOverride](bool bOk, const FString& Message, const TArray<uint8>& Bytes)
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

			switch (FAmbarImporter::ModeFor(Asset))
			{
			case EAmbarImportMode::CopyToContent:
				CopyIntoContent(Asset, Bytes, Done);
				return;
			case EAmbarImportMode::PlainFile:
				CopyAsPlainFile(Asset, Bytes, Done);
				return;
			default:
				break;
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

			const FString Folder = FolderOverride.IsEmpty() ? FAmbarImporter::DestinationFolder(Asset) : FolderOverride;
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
				// No importer took it (an .ogg, an .aseprite, a .psd this engine will not read): still
				// worth having, so it is copied as a plain file, and the message says which happened.
				CopyAsPlainFile(Asset, Bytes, [Done, Asset](const FAmbarImportResult& R)
				{
					FAmbarImportResult Out = R;
					if (R.bOk)
					{
						Out.Message = FString::Printf(TEXT("Unreal has no importer for .%s; %s instead"), *Asset.Ext,
							*R.Message.Replace(TEXT("Copied"), TEXT("copied the file to"), ESearchCase::CaseSensitive));
					}
					Done(Out);
				});
				return;
			}

			if (UAmbarSettings::Get()->bPixelArtDefaults && Asset.bPixelArt)
			{
				TArray<UPackage*> Changed;
				for (const FString& ObjectPath : Imported)
				{
					if (UTexture2D* Texture = LoadObject<UTexture2D>(nullptr, *ObjectPath))
					{
						ApplyPixelArtDefaults(Texture);
						Changed.AddUnique(Texture->GetOutermost());
					}
				}
				// The import saved before these settings changed; save again so they reach the disk.
				if (Changed.Num() > 0)
				{
					UEditorLoadingAndSavingUtils::SavePackages(Changed, /*bOnlyDirty*/ true);
				}
			}

			const FString PackagePath = FPackageName::ObjectPathToPackageName(Imported[0]);
			Finish(Asset, PackagePath, Folder, TEXT("Imported"), Done);
		},
		OnProgress);
}
