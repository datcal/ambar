#include "AmbarTypes.h"

#include "Policies/PrettyJsonPrintPolicy.h"
#include "Serialization/JsonReader.h"
#include "Serialization/JsonSerializer.h"
#include "Serialization/JsonWriter.h"

namespace AmbarJson
{
	FString Str(const TSharedPtr<FJsonObject>& Json, const TCHAR* Key)
	{
		FString Out;
		if (Json.IsValid())
		{
			Json->TryGetStringField(Key, Out);
		}
		return Out;
	}

	double Num(const TSharedPtr<FJsonObject>& Json, const TCHAR* Key)
	{
		double Out = 0.0;
		if (Json.IsValid())
		{
			Json->TryGetNumberField(Key, Out);
		}
		return Out;
	}

	bool Bool(const TSharedPtr<FJsonObject>& Json, const TCHAR* Key)
	{
		bool Out = false;
		if (Json.IsValid())
		{
			Json->TryGetBoolField(Key, Out);
		}
		return Out;
	}

	TSharedPtr<FJsonObject> Parse(const FString& Text)
	{
		TSharedPtr<FJsonObject> Object;
		const TSharedRef<TJsonReader<>> Reader = TJsonReaderFactory<>::Create(Text);
		if (!FJsonSerializer::Deserialize(Reader, Object) || !Object.IsValid())
		{
			return nullptr;
		}
		return Object;
	}

	FString Write(const TSharedRef<FJsonObject>& Json)
	{
		FString Out;
		const TSharedRef<TJsonWriter<TCHAR, TPrettyJsonPrintPolicy<TCHAR>>> Writer =
			TJsonWriterFactory<TCHAR, TPrettyJsonPrintPolicy<TCHAR>>::Create(&Out);
		FJsonSerializer::Serialize(Json, Writer);
		return Out;
	}
}

FString FAmbarAsset::SizeText() const
{
	if (Size < 1024)
	{
		return FString::Printf(TEXT("%lld B"), Size);
	}
	if (Size < 1024 * 1024)
	{
		return FString::Printf(TEXT("%.1f KB"), Size / 1024.0);
	}
	if (Size < 1024LL * 1024 * 1024)
	{
		return FString::Printf(TEXT("%.1f MB"), Size / (1024.0 * 1024.0));
	}
	return FString::Printf(TEXT("%.2f GB"), Size / (1024.0 * 1024.0 * 1024.0));
}

FAmbarAsset FAmbarAsset::FromJson(const TSharedPtr<FJsonObject>& Json)
{
	FAmbarAsset A;
	if (!Json.IsValid())
	{
		return A;
	}
	A.Id = static_cast<int64>(AmbarJson::Num(Json, TEXT("id")));
	A.Filename = AmbarJson::Str(Json, TEXT("filename"));
	A.Ext = AmbarJson::Str(Json, TEXT("ext"));
	A.Kind = AmbarJson::Str(Json, TEXT("kind"));
	A.Sha256 = AmbarJson::Str(Json, TEXT("sha256"));
	A.Path = AmbarJson::Str(Json, TEXT("path"));
	A.DeriveState = AmbarJson::Str(Json, TEXT("derive_state"));
	A.Size = static_cast<int64>(AmbarJson::Num(Json, TEXT("size")));
	A.Width = static_cast<int32>(AmbarJson::Num(Json, TEXT("width")));
	A.Height = static_cast<int32>(AmbarJson::Num(Json, TEXT("height")));
	A.TriCount = static_cast<int32>(AmbarJson::Num(Json, TEXT("tri_count")));
	A.FrameCount = static_cast<int32>(AmbarJson::Num(Json, TEXT("frame_count")));
	A.VariantCount = static_cast<int32>(AmbarJson::Num(Json, TEXT("variant_count")));
	A.DurationMs = static_cast<int32>(AmbarJson::Num(Json, TEXT("duration_ms")));
	A.bPixelArt = AmbarJson::Bool(Json, TEXT("is_pixel_art"));

	const TSharedPtr<FJsonObject>* Pack = nullptr;
	if (Json->TryGetObjectField(TEXT("pack"), Pack) && Pack && Pack->IsValid())
	{
		A.PackName = AmbarJson::Str(*Pack, TEXT("name"));
		A.PackSlug = AmbarJson::Str(*Pack, TEXT("slug"));
	}
	return A;
}

FAmbarDetail FAmbarDetail::FromJson(const TSharedPtr<FJsonObject>& Json)
{
	FAmbarDetail D;
	if (!Json.IsValid())
	{
		return D;
	}

	const TSharedPtr<FJsonObject>* AssetObj = nullptr;
	if (Json->TryGetObjectField(TEXT("asset"), AssetObj) && AssetObj)
	{
		D.Asset = FAmbarAsset::FromJson(*AssetObj);
	}

	const TArray<TSharedPtr<FJsonValue>>* TagValues = nullptr;
	if (Json->TryGetArrayField(TEXT("tags"), TagValues) && TagValues)
	{
		for (const TSharedPtr<FJsonValue>& V : *TagValues)
		{
			FString Tag;
			if (V.IsValid() && V->TryGetString(Tag))
			{
				D.Tags.Add(Tag);
			}
		}
	}

	const TArray<TSharedPtr<FJsonValue>>* VariantValues = nullptr;
	if (Json->TryGetArrayField(TEXT("variants"), VariantValues) && VariantValues)
	{
		for (const TSharedPtr<FJsonValue>& V : *VariantValues)
		{
			const TSharedPtr<FJsonObject>* Obj = nullptr;
			if (V.IsValid() && V->TryGetObject(Obj) && Obj)
			{
				D.Variants.Add(FAmbarAsset::FromJson(*Obj));
			}
		}
	}

	const TSharedPtr<FJsonObject>* Prov = nullptr;
	if (Json->TryGetObjectField(TEXT("provenance"), Prov) && Prov)
	{
		D.License = AmbarJson::Str(*Prov, TEXT("license"));
		D.Author = AmbarJson::Str(*Prov, TEXT("author"));
		D.SourceUrl = AmbarJson::Str(*Prov, TEXT("source_url"));
	}
	return D;
}

FAmbarUse FAmbarUse::FromJson(const TSharedPtr<FJsonObject>& Json)
{
	FAmbarUse U;
	U.Id = static_cast<int64>(AmbarJson::Num(Json, TEXT("id")));
	U.AssetId = static_cast<int64>(AmbarJson::Num(Json, TEXT("asset_id")));
	U.ResPath = AmbarJson::Str(Json, TEXT("res_path"));
	U.Filename = AmbarJson::Str(Json, TEXT("filename"));
	U.Kind = AmbarJson::Str(Json, TEXT("kind"));
	U.Pack = AmbarJson::Str(Json, TEXT("pack"));
	U.bOutdated = AmbarJson::Bool(Json, TEXT("outdated"));
	U.bMissing = AmbarJson::Bool(Json, TEXT("missing"));
	return U;
}
