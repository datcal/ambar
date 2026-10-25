#include "AmbarClient.h"

#include "AmbarSettings.h"
#include "GenericPlatform/GenericPlatformHttp.h"
#include "HttpModule.h"
#include "Interfaces/IHttpRequest.h"
#include "Interfaces/IHttpResponse.h"

namespace
{
	FString Enc(const FString& S)
	{
		return FGenericPlatformHttp::UrlEncode(S);
	}

	TSharedRef<IHttpRequest, ESPMode::ThreadSafe> MakeRequest(const FString& Url, const TCHAR* Verb, float TimeoutSeconds = 30.f)
	{
		const UAmbarSettings* Settings = UAmbarSettings::Get();
		TSharedRef<IHttpRequest, ESPMode::ThreadSafe> Req = FHttpModule::Get().CreateRequest();
		Req->SetURL(Url);
		Req->SetVerb(Verb);
		Req->SetHeader(TEXT("Authorization"), TEXT("Bearer ") + Settings->ApiToken.TrimStartAndEnd());
		Req->SetHeader(TEXT("Accept"), TEXT("application/json, image/png, */*"));
		// A Cloudflare tunnel protected by Cloudflare Access wants a service token on every request.
		if (!Settings->AccessClientId.IsEmpty() && !Settings->AccessClientSecret.IsEmpty())
		{
			Req->SetHeader(TEXT("CF-Access-Client-Id"), Settings->AccessClientId.TrimStartAndEnd());
			Req->SetHeader(TEXT("CF-Access-Client-Secret"), Settings->AccessClientSecret.TrimStartAndEnd());
		}
		if (TimeoutSeconds > 0.f)
		{
			Req->SetTimeout(TimeoutSeconds);
		}
		return Req;
	}

	/** Turns a response into a sentence. Empty when the request succeeded. */
	FString Explain(const TSharedPtr<IHttpResponse, ESPMode::ThreadSafe>& Resp, bool bConnected)
	{
		if (!bConnected || !Resp.IsValid())
		{
			return TEXT("Cannot reach the server. Check the address, and that Ambar (or the tunnel) is running.");
		}
		const int32 Code = Resp->GetResponseCode();
		if (Code >= 200 && Code < 300)
		{
			return FString();
		}

		// A tunnel with Cloudflare Access in front answers with a login page instead of the API.
		const FString Type = Resp->GetContentType();
		const bool bHtml = Type.Contains(TEXT("text/html"));
		if ((Code == 401 || Code == 403 || Code == 302) && bHtml)
		{
			return TEXT("A web page came back instead of the API. If the tunnel uses Cloudflare Access, add a service token under Cloudflare Access in the Ambar settings.");
		}
		switch (Code)
		{
		case 401:
			return TEXT("Unauthorised. The API token is missing or wrong (Ambar web UI > Settings > API tokens).");
		case 403:
			return TEXT("Forbidden. The token exists but is not allowed to do that (recording imports needs a token with write).");
		case 404:
			return TEXT("Not found. Check that the address points at the root of Ambar.");
		case 503:
			return TEXT("Not ready yet. The server has not generated this preview.");
		default:
			return FString::Printf(TEXT("The server answered %d."), Code);
		}
	}

	void GetJson(const FString& Url, FAmbarJsonCallback Done)
	{
		if (UAmbarSettings::Get()->BaseUrl().IsEmpty())
		{
			Done(false, TEXT("No server address configured."), nullptr);
			return;
		}
		TSharedRef<IHttpRequest, ESPMode::ThreadSafe> Req = MakeRequest(Url, TEXT("GET"));
		Req->OnProcessRequestComplete().BindLambda(
			[Done](FHttpRequestPtr, FHttpResponsePtr Resp, bool bConnected)
			{
				const FString Problem = Explain(Resp, bConnected);
				if (!Problem.IsEmpty())
				{
					Done(false, Problem, nullptr);
					return;
				}
				const TSharedPtr<FJsonObject> Json = AmbarJson::Parse(Resp->GetContentAsString());
				if (!Json.IsValid())
				{
					Done(false, TEXT("The server answered with something that is not JSON. Is the address Ambar's?"), nullptr);
					return;
				}
				Done(true, FString(), Json);
			});
		Req->ProcessRequest();
	}

	void SendJson(const FString& Url, const TCHAR* Verb, const FString& Body, FAmbarDoneCallback Done)
	{
		TSharedRef<IHttpRequest, ESPMode::ThreadSafe> Req = MakeRequest(Url, Verb);
		if (!Body.IsEmpty())
		{
			Req->SetHeader(TEXT("Content-Type"), TEXT("application/json"));
			Req->SetContentAsString(Body);
		}
		Req->OnProcessRequestComplete().BindLambda(
			[Done](FHttpRequestPtr, FHttpResponsePtr Resp, bool bConnected)
			{
				const FString Problem = Explain(Resp, bConnected);
				Done(Problem.IsEmpty(), Problem);
			});
		Req->ProcessRequest();
	}
}

FString FAmbarClient::BaseUrl()
{
	return UAmbarSettings::Get()->BaseUrl();
}

FString FAmbarClient::ThumbUrl(int64 AssetId)
{
	// PNG, because the editor's image wrapper has no WebP decoder; the server transcodes.
	return FString::Printf(TEXT("%s/api/v1/assets/%lld/thumb?size=2x&format=png"), *BaseUrl(), AssetId);
}

FString FAmbarClient::PreviewUrl(int64 AssetId)
{
	return FString::Printf(TEXT("%s/api/v1/assets/%lld/preview.webp?format=png"), *BaseUrl(), AssetId);
}

FString FAmbarClient::WebUrl(int64 AssetId)
{
	return FString::Printf(TEXT("%s/assets/%lld"), *BaseUrl(), AssetId);
}

void FAmbarClient::Ping(FAmbarJsonCallback Done)
{
	GetJson(BaseUrl() + TEXT("/api/v1/ping"), MoveTemp(Done));
}

void FAmbarClient::Sorts(FAmbarJsonCallback Done)
{
	GetJson(BaseUrl() + TEXT("/api/v1/sorts"), MoveTemp(Done));
}

void FAmbarClient::Search(const FAmbarQuery& Query, FAmbarJsonCallback Done)
{
	// group=1: the PNG, PSD and ASEPRITE of one sprite are one tile, not three.
	FString Url = FString::Printf(TEXT("%s/api/v1/search?group=1&q=%s&page=%d&limit=%d"),
		*BaseUrl(), *Enc(Query.Text), FMath::Max(1, Query.Page), FMath::Max(1, Query.PageSize));
	if (!Query.Kind.IsEmpty())
	{
		Url += TEXT("&kind=") + Enc(Query.Kind);
	}
	if (!Query.Sort.IsEmpty())
	{
		Url += TEXT("&sort=") + Enc(Query.Sort);
	}
	if (!Query.Dir.IsEmpty())
	{
		Url += TEXT("&dir=") + Enc(Query.Dir);
	}
	GetJson(Url, MoveTemp(Done));
}

void FAmbarClient::Folders(const FString& Path, FAmbarJsonCallback Done)
{
	GetJson(BaseUrl() + TEXT("/api/v1/folders?path=") + Enc(Path), MoveTemp(Done));
}

void FAmbarClient::GetAsset(int64 AssetId, FAmbarJsonCallback Done)
{
	GetJson(FString::Printf(TEXT("%s/api/v1/assets/%lld"), *BaseUrl(), AssetId), MoveTemp(Done));
}

void FAmbarClient::FetchBytes(const FString& Url, FAmbarBytesCallback Done)
{
	TSharedRef<IHttpRequest, ESPMode::ThreadSafe> Req = MakeRequest(Url, TEXT("GET"), 60.f);
	Req->OnProcessRequestComplete().BindLambda(
		[Done](FHttpRequestPtr, FHttpResponsePtr Resp, bool bConnected)
		{
			const FString Problem = Explain(Resp, bConnected);
			if (!Problem.IsEmpty())
			{
				Done(false, Problem, TArray<uint8>());
				return;
			}
			Done(true, FString(), Resp->GetContent());
		});
	Req->ProcessRequest();
}

void FAmbarClient::DownloadFile(int64 AssetId, FAmbarBytesCallback Done, TFunction<void(uint64, uint64)> Progress)
{
	// No overall timeout: a 300 MB model over a tunnel legitimately takes a while.
	TSharedRef<IHttpRequest, ESPMode::ThreadSafe> Req =
		MakeRequest(FString::Printf(TEXT("%s/api/v1/assets/%lld/file"), *BaseUrl(), AssetId), TEXT("GET"), 0.f);
	if (Progress)
	{
		Req->OnRequestProgress64().BindLambda(
			[Progress](FHttpRequestPtr, uint64, uint64 Received) { Progress(Received, 0); });
	}
	Req->OnProcessRequestComplete().BindLambda(
		[Done](FHttpRequestPtr, FHttpResponsePtr Resp, bool bConnected)
		{
			const FString Problem = Explain(Resp, bConnected);
			if (!Problem.IsEmpty())
			{
				Done(false, Problem, TArray<uint8>());
				return;
			}
			Done(true, FString(), Resp->GetContent());
		});
	Req->ProcessRequest();
}

void FAmbarClient::RecordUse(const FString& ProjectUuid, const FString& ProjectName, const FAmbarAsset& Asset,
	const FString& ResPath, FAmbarDoneCallback Done)
{
	const TSharedRef<FJsonObject> Body = MakeShared<FJsonObject>();
	Body->SetNumberField(TEXT("asset_id"), static_cast<double>(Asset.Id));
	Body->SetStringField(TEXT("res_path"), ResPath);
	Body->SetStringField(TEXT("sha256"), Asset.Sha256);
	Body->SetStringField(TEXT("project_name"), ProjectName);
	SendJson(FString::Printf(TEXT("%s/api/v1/projects/%s/uses"), *BaseUrl(), *Enc(ProjectUuid)), TEXT("POST"),
		AmbarJson::Write(Body), MoveTemp(Done));
}

void FAmbarClient::ProjectUses(const FString& ProjectUuid, FAmbarJsonCallback Done)
{
	GetJson(FString::Printf(TEXT("%s/api/v1/projects/%s/uses"), *BaseUrl(), *Enc(ProjectUuid)), MoveTemp(Done));
}

void FAmbarClient::RemoveUse(const FString& ProjectUuid, int64 UseId, FAmbarDoneCallback Done)
{
	SendJson(FString::Printf(TEXT("%s/api/v1/projects/%s/uses/%lld"), *BaseUrl(), *Enc(ProjectUuid), UseId),
		TEXT("DELETE"), FString(), MoveTemp(Done));
}

void FAmbarClient::Credits(const FString& ProjectUuid, FAmbarBytesCallback Done)
{
	FetchBytes(FString::Printf(TEXT("%s/api/v1/projects/%s/credits.md"), *BaseUrl(), *Enc(ProjectUuid)), MoveTemp(Done));
}
