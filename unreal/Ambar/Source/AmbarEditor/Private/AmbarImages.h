#pragma once

#include "CoreMinimal.h"
#include "Engine/Texture2D.h"
#include "Styling/SlateBrush.h"
#include "UObject/StrongObjectPtr.h"

/**
 * Pictures for the browser: memory first, then the disk cache, then the server.
 *
 * Only a handful of requests run at once — a page is up to 200 tiles and the library is probably on a
 * NAS behind a tunnel, and 200 simultaneous connections is how a library ends up feeling broken. Tiles
 * ask for what they show; `Find` is what they poll each frame, and it is a map lookup.
 */
class FAmbarImages : public TSharedFromThis<FAmbarImages>
{
public:
	/** The decoded picture, or null while it is still on its way. */
	const FSlateBrush* Find(const FString& Key);

	/**
	 * Starts loading `Url` as `Key` unless it is loaded, loading or failed recently.
	 * `DiskCategory`/`DiskKey` name the cache file ("thumbs"/"42-ab12cd34").
	 */
	void Request(const FString& Key, const FString& Url, const FString& DiskCategory, const FString& DiskKey, bool bNearest);

	/** Forgets everything in memory (after the disk cache was cleared). */
	void Reset();

private:
	struct FEntry
	{
		TStrongObjectPtr<UTexture2D> Texture;
		TSharedPtr<FSlateBrush> Brush;
		uint64 LastUsed = 0;
	};
	struct FPending
	{
		FString Key;
		FString Url;
		FString DiskCategory;
		FString DiskKey;
		bool bNearest = false;
	};

	void Pump();
	void Finish(const FPending& Job, bool bOk, const TArray<uint8>& Png);
	bool Store(const FString& Key, const TArray<uint8>& Png, bool bNearest);
	void Evict();

	TMap<FString, FEntry> Entries;
	TMap<FString, double> Failed;
	TSet<FString> Queued;
	TArray<FPending> Queue;
	int32 InFlight = 0;
	uint64 Tick = 0;
	/** Bumped by Reset so a response that was in flight does not land in the new generation. */
	int32 Generation = 0;

	static constexpr int32 MaxInFlight = 6;
	static constexpr int32 MaxEntries = 600;
};
