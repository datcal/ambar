#pragma once

#include "CoreMinimal.h"
#include "AmbarTypes.h"

/** (ok, message, json). On failure `message` is a sentence that says what to do about it. */
using FAmbarJsonCallback = TFunction<void(bool, const FString&, const TSharedPtr<FJsonObject>&)>;
/** (ok, message, bytes). */
using FAmbarBytesCallback = TFunction<void(bool, const FString&, const TArray<uint8>&)>;
/** (ok, message). */
using FAmbarDoneCallback = TFunction<void(bool, const FString&)>;

/**
 * Thin async client for the Ambar JSON API. Every call reads the user's settings at the moment it
 * is made, so changing the server or the token takes effect without a restart. Callbacks run on the
 * game thread.
 */
namespace FAmbarClient
{
	FString BaseUrl();

	// URLs, for the image loader and the "open in browser" button.
	FString ThumbUrl(int64 AssetId);
	FString PreviewUrl(int64 AssetId);
	FString WebUrl(int64 AssetId);

	void Ping(FAmbarJsonCallback Done);
	void Sorts(FAmbarJsonCallback Done);
	void Search(const FAmbarQuery& Query, FAmbarJsonCallback Done);
	void Folders(const FString& Path, FAmbarJsonCallback Done);
	void GetAsset(int64 AssetId, FAmbarJsonCallback Done);

	/** Downloads an asset's original bytes. Progress is (bytes received, bytes expected or 0). */
	void DownloadFile(int64 AssetId, FAmbarBytesCallback Done, TFunction<void(uint64, uint64)> Progress = nullptr);
	/** Fetches any URL under the server (thumbnails, previews) with the same credentials. */
	void FetchBytes(const FString& Url, FAmbarBytesCallback Done);

	void RecordUse(const FString& ProjectUuid, const FString& ProjectName, const FAmbarAsset& Asset,
		const FString& ResPath, FAmbarDoneCallback Done);
	void ProjectUses(const FString& ProjectUuid, FAmbarJsonCallback Done);
	void RemoveUse(const FString& ProjectUuid, int64 UseId, FAmbarDoneCallback Done);
	void Credits(const FString& ProjectUuid, FAmbarBytesCallback Done);
}
