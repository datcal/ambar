#pragma once

#include "CoreMinimal.h"
#include "AmbarTypes.h"

struct FAmbarImportResult
{
	bool bOk = false;
	FString Message;
	/** The content folder the asset went into, e.g. /Game/Ambar/image/kenney-pack. */
	FString Folder;
};

namespace FAmbarImporter
{
	/** The folder an asset is imported into: <ImportRoot>/<kind>/<pack>. */
	FString DestinationFolder(const FAmbarAsset& Asset);

	/**
	 * Downloads the original, checks it against the hash the server advertised, imports it with the
	 * editor's own importers (so glTF, FBX, PNG, WAV ... behave exactly as dragging the file in would),
	 * records it in the project manifest and tells the server. Progress is 0..1.
	 */
	void Import(const FAmbarAsset& Asset, TFunction<void(const FAmbarImportResult&)> Done,
		TFunction<void(float)> Progress = nullptr);
}
