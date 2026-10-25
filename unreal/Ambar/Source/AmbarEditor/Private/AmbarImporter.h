#pragma once

#include "CoreMinimal.h"
#include "AmbarTypes.h"

struct FAmbarImportResult
{
	bool bOk = false;
	FString Message;
	/** The content folder the asset went into, e.g. /Game/Ambar/2d/kenney-pack/PNG. Empty for a file
	 *  copied outside Content (code, notes) — the Content Browser has nothing to show for those. */
	FString Folder;
};

/** How an asset reaches the project, decided by what it is. */
enum class EAmbarImportMode : uint8
{
	/** Through the editor's own importers: models, images, audio, fonts. */
	Import,
	/** An Unreal package (.uasset/.umap): copied into Content where it can resolve its references. */
	CopyToContent,
	/** Anything else — code, configs, notes: copied as a plain file under <Project>/Ambar/. */
	PlainFile,
};

namespace FAmbarImporter
{
	/** What Import will do with this asset. */
	EAmbarImportMode ModeFor(const FAmbarAsset& Asset);

	/**
	 * The content folder an imported asset goes into: <ImportRoot>/<the asset's folder in the library>.
	 * Mirroring the library keeps every file apart — a vendor directory holding 180 packs is one Ambar
	 * pack, and two of its packs both have an SM_Prop_Raft_01.
	 */
	FString DestinationFolder(const FAmbarAsset& Asset);

	/**
	 * Downloads the original, checks it against the hash the server advertised, brings it into the project
	 * the way ModeFor says, records it in the project manifest and tells the server. Progress is 0..1.
	 *
	 * `Folder`, when given, is the content folder to import into instead of DestinationFolder — an update
	 * passes the folder the asset already lives in, so it lands on top of itself and references survive.
	 */
	void Import(const FAmbarAsset& Asset, TFunction<void(const FAmbarImportResult&)> Done,
		TFunction<void(float)> Progress = nullptr, const FString& Folder = FString());
}
