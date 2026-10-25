#pragma once

#include "CoreMinimal.h"

/**
 * The local cache: folder names, thumbnails and previews, kept on disk so that opening a folder you
 * have seen before is instant and works with the server unreachable.
 *
 * Layout: <root>/<server hash>/{folders,thumbs,previews}/<key>.bin — one sub-folder per server, so
 * pointing the plugin at a second library never shows the first one's pictures. Thumbnails are keyed
 * by asset id and content hash, so an entry can never be stale, only unused. Nothing in here is
 * precious: delete the folder (or use the Cache menu) and it refills.
 */
namespace FAmbarCache
{
	/** The folder for the configured server. */
	FString Root();
	/** The folder the user can open and delete by hand. */
	FString BaseRoot();

	bool Read(const FString& Category, const FString& Key, TArray<uint8>& OutBytes);
	void Write(const FString& Category, const FString& Key, const TArray<uint8>& Bytes);

	/** Number of files and total bytes in the whole cache folder (every server). */
	void Stats(int32& OutFiles, int64& OutBytes);

	void ClearImages();
	void ClearFolders();
	void ClearAll();

	/** Deletes the least recently used images until the cache is under its limit. */
	void Trim();
}
