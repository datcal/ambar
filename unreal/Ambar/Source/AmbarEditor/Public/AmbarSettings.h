#pragma once

#include "CoreMinimal.h"
#include "Engine/DeveloperSettings.h"
#include "AmbarSettings.generated.h"

/**
 * Per-user editor settings. Stored in the user's EditorSettings.ini — outside any project — so
 * the API token is never committed and every person on the team uses their own.
 */
UCLASS(config = EditorSettings, meta = (DisplayName = "Ambar"))
class AMBAREDITOR_API UAmbarSettings : public UDeveloperSettings
{
	GENERATED_BODY()

public:
	UAmbarSettings();

	virtual FName GetContainerName() const override { return TEXT("Editor"); }
	virtual FName GetCategoryName() const override { return TEXT("Plugins"); }

	static UAmbarSettings* Get() { return GetMutableDefault<UAmbarSettings>(); }

	/** Address of the Ambar server, e.g. http://nas:8973 or https://ambar.example.com (a Cloudflare tunnel). */
	UPROPERTY(config, EditAnywhere, Category = "Server")
	FString ServerUrl;

	/** API token from Ambar's Settings > API tokens page. Tick "write" so imports can be recorded. */
	UPROPERTY(config, EditAnywhere, Category = "Server", meta = (PasswordField = true))
	FString ApiToken;

	/** Only needed when the tunnel is protected by Cloudflare Access: the service token's client ID. */
	UPROPERTY(config, EditAnywhere, Category = "Cloudflare Access")
	FString AccessClientId;

	/** Cloudflare Access service token secret. */
	UPROPERTY(config, EditAnywhere, Category = "Cloudflare Access", meta = (PasswordField = true))
	FString AccessClientSecret;

	/** Where imported assets go. Each asset lands in <root>/<kind>/<pack>. */
	UPROPERTY(config, EditAnywhere, Category = "Import")
	FString ImportRoot;

	/** Pixel-art textures (as detected by the server) get nearest filtering, no mipmaps, UI compression. */
	UPROPERTY(config, EditAnywhere, Category = "Import")
	bool bPixelArtDefaults;

	/** Show the new folder in the Content Browser after an import. */
	UPROPERTY(config, EditAnywhere, Category = "Import")
	bool bSyncContentBrowser;

	/** Keep thumbnails and previews on disk, so the second visit to a folder is instant and works offline. */
	UPROPERTY(config, EditAnywhere, Category = "Cache")
	bool bCacheImages;

	/** Remember folder names on disk and show them immediately, refreshing in the background. */
	UPROPERTY(config, EditAnywhere, Category = "Cache")
	bool bCacheFolders;

	/** Thumbnails and previews are evicted, least recently used first, beyond this size. */
	UPROPERTY(config, EditAnywhere, Category = "Cache", meta = (ClampMin = "16", UIMin = "16", UIMax = "8192", Units = "MB"))
	int32 MaxCacheSizeMB;

	/** Empty means <user settings>/AmbarUnreal/Cache. Each server gets its own sub-folder. */
	UPROPERTY(config, EditAnywhere, Category = "Cache")
	FString CacheDirectoryOverride;

	/** Thumbnail edge length in pixels (also set by the slider in the browser). */
	UPROPERTY(config, EditAnywhere, Category = "Browser", meta = (ClampMin = "64", ClampMax = "320"))
	int32 TileSize;

	/** Assets per page. */
	UPROPERTY(config, EditAnywhere, Category = "Browser", meta = (ClampMin = "12", ClampMax = "200"))
	int32 PageSize;

	bool IsConfigured() const { return !ServerUrl.TrimStartAndEnd().IsEmpty() && !ApiToken.TrimStartAndEnd().IsEmpty(); }

	/** The server URL without whitespace or a trailing slash. */
	FString BaseUrl() const;
};
