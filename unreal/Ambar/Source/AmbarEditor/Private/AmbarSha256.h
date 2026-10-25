#pragma once

#include "CoreMinimal.h"

namespace AmbarHash
{
	/** Lower-case hex SHA-256, to check a download against the hash the server listed. */
	FString Sha256Hex(const uint8* Data, uint64 Length);
}
