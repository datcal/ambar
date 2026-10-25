#include "AmbarSettings.h"

UAmbarSettings::UAmbarSettings()
	: ServerUrl(TEXT("http://localhost:8973"))
	, ImportRoot(TEXT("/Game/Ambar"))
	, bPixelArtDefaults(true)
	, bSyncContentBrowser(true)
	, bCacheImages(true)
	, bCacheFolders(true)
	, MaxCacheSizeMB(512)
	, TileSize(150)
	, PageSize(60)
{
}

FString UAmbarSettings::BaseUrl() const
{
	FString Url = ServerUrl.TrimStartAndEnd();
	while (Url.EndsWith(TEXT("/")))
	{
		Url.LeftChopInline(1);
	}
	return Url;
}
