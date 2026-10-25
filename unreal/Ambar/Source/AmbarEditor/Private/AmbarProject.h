#pragma once

#include "CoreMinimal.h"
#include "AmbarTypes.h"

/**
 * Which Ambar assets this project holds. Two small files in <Project>/.ambar/ — commit them:
 *
 *   project.json   { "uuid": ..., "name": ... }   identifies the project to the server. A UUID, never a
 *                  path, because two people check the project out in different places.
 *   manifest.json  asset id -> { res_path, sha256, filename, pack }   what was imported, so everyone's
 *                  editor knows what is already here and the server can build CREDITS.md.
 *
 * The format is the Godot plugin's, so one server serves both engines.
 */
namespace FAmbarProject
{
	FString Dir();
	FString Uuid();
	FString Name();

	struct FEntry
	{
		int64 AssetId = 0;
		FString ResPath;
		FString Sha256;
		FString Filename;
		FString Pack;
	};

	/** Everything in the manifest. */
	TArray<FEntry> Entries();

	/** The asset ids in the manifest. */
	TSet<int64> ImportedIds();

	/** Adds or replaces one entry. `PackagePath` is the long package name of what was imported. */
	void Record(const FAmbarAsset& Asset, const FString& PackagePath);
	void Forget(int64 AssetId);

	/** Where the generated credits file goes. */
	FString CreditsPath();
}
