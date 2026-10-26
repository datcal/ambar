#pragma once

#include "CoreMinimal.h"
#include "Containers/Ticker.h"
#include "Interfaces/IHttpRequest.h"

/**
 * Imports an AI assistant asked for.
 *
 * The assistant talks to the Ambar server's MCP endpoint, and the server cannot reach into this project.
 * It keeps the request, and this editor asks for it: a long poll the server holds open for up to
 * 25 seconds and answers as soon as there is something, asked again straight away. Each asset goes
 * through the same importer as the browser's Import button (manifest, credits, folder mirroring), and the
 * package paths go back to the server, which hands them to the assistant.
 */
class FAmbarRequests : public TSharedFromThis<FAmbarRequests>
{
public:
	void Start();
	void Stop();

private:
	struct FRequest
	{
		int64 Id = 0;
		TArray<int64> AssetIds;
		FString Folder;
		FString Via;
	};

	void Poll();
	void PollLater(float Seconds);
	void RunNext();
	void Run(const FRequest& Request);

	TSharedPtr<IHttpRequest, ESPMode::ThreadSafe> InFlight;
	FTSTicker::FDelegateHandle Later;
	TArray<FRequest> Queue;
	bool bRunning = false;
	/** A request's imports are under way; the poll resumes when they are done. */
	bool bBusy = false;
};
