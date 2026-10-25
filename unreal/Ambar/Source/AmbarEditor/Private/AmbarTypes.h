#pragma once

#include "CoreMinimal.h"
#include "Dom/JsonObject.h"

/** One asset as the server describes it (the `assetJSON` wire shape). */
struct FAmbarAsset
{
	int64 Id = 0;
	FString Filename;
	FString Ext;
	FString Kind;
	FString Sha256;
	FString Path;
	FString PackName;
	FString PackSlug;
	FString DeriveState;
	int64 Size = 0;
	int32 Width = 0;
	int32 Height = 0;
	int32 TriCount = 0;
	int32 FrameCount = 0;
	int32 VariantCount = 0;
	int32 DurationMs = 0;
	bool bPixelArt = false;

	bool IsValid() const { return Id > 0; }
	FString SizeText() const;
	static FAmbarAsset FromJson(const TSharedPtr<FJsonObject>& Json);
};

/** What the browser is showing: the parameters of GET /api/v1/search. */
struct FAmbarQuery
{
	FString Text;
	FString Kind;
	FString Sort;
	FString Dir;
	int32 Page = 1;
	int32 PageSize = 60;
};

/** The extra things GET /api/v1/assets/{id} returns for the inspector. */
struct FAmbarDetail
{
	FAmbarAsset Asset;
	TArray<FString> Tags;
	TArray<FAmbarAsset> Variants;
	FString License;
	FString Author;
	FString SourceUrl;

	static FAmbarDetail FromJson(const TSharedPtr<FJsonObject>& Json);
};

/** One row of GET /api/v1/projects/{uuid}/uses. */
struct FAmbarUse
{
	int64 Id = 0;
	int64 AssetId = 0;
	FString ResPath;
	FString Filename;
	FString Kind;
	FString Pack;
	bool bOutdated = false;
	bool bMissing = false;

	static FAmbarUse FromJson(const TSharedPtr<FJsonObject>& Json);
};

namespace AmbarJson
{
	FString Str(const TSharedPtr<FJsonObject>& Json, const TCHAR* Key);
	double Num(const TSharedPtr<FJsonObject>& Json, const TCHAR* Key);
	bool Bool(const TSharedPtr<FJsonObject>& Json, const TCHAR* Key);
	TSharedPtr<FJsonObject> Parse(const FString& Text);
	FString Write(const TSharedRef<FJsonObject>& Json);
}
