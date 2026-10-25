#pragma once

#include "CoreMinimal.h"
#include "Brushes/SlateRoundedBoxBrush.h"
#include "Fonts/SlateFontInfo.h"
#include "Styling/CoreStyle.h"

/** The look of the browser: a dark, amber-accented palette, rounded cards, and a few font sizes. */
namespace AmbarUI
{
	inline FLinearColor Srgb(uint8 R, uint8 G, uint8 B, uint8 A = 255)
	{
		return FLinearColor::FromSRGBColor(FColor(R, G, B, A));
	}

	inline FLinearColor Backdrop() { return Srgb(0x16, 0x18, 0x1c); }
	inline FLinearColor Panel() { return Srgb(0x1f, 0x22, 0x27); }
	inline FLinearColor Card() { return Srgb(0x28, 0x2c, 0x33); }
	inline FLinearColor CardHover() { return Srgb(0x32, 0x37, 0x40); }
	inline FLinearColor Accent() { return Srgb(0xe8, 0xa3, 0x3d); }
	inline FLinearColor AccentDim() { return Srgb(0xe8, 0xa3, 0x3d, 0x33); }
	inline FLinearColor Text() { return Srgb(0xe6, 0xe8, 0xeb); }
	inline FLinearColor TextDim() { return Srgb(0x9a, 0xa3, 0xad); }
	inline FLinearColor Good() { return Srgb(0x5f, 0xc2, 0x7a); }
	inline FLinearColor Warn() { return Srgb(0xe8, 0xa3, 0x3d); }
	inline FLinearColor Bad() { return Srgb(0xe0, 0x6b, 0x6b); }

	inline const FSlateBrush* BackdropBrush()
	{
		static const FSlateRoundedBoxBrush B(Backdrop(), 0.f, FLinearColor::Transparent, 0.f);
		return &B;
	}
	inline const FSlateBrush* PanelBrush()
	{
		static const FSlateRoundedBoxBrush B(Panel(), 8.f, FLinearColor::Transparent, 0.f);
		return &B;
	}
	inline const FSlateBrush* CardBrush()
	{
		static const FSlateRoundedBoxBrush B(Card(), 8.f, FLinearColor::Transparent, 0.f);
		return &B;
	}
	inline const FSlateBrush* CardHoverBrush()
	{
		static const FSlateRoundedBoxBrush B(CardHover(), 8.f, FLinearColor::Transparent, 0.f);
		return &B;
	}
	inline const FSlateBrush* CardSelectedBrush()
	{
		static const FSlateRoundedBoxBrush B(AccentDim(), 8.f, Accent(), 2.f);
		return &B;
	}
	inline const FSlateBrush* PillBrush()
	{
		static const FSlateRoundedBoxBrush B(Srgb(0x0e, 0x0f, 0x12, 0xcc), 9.f, FLinearColor::Transparent, 0.f);
		return &B;
	}
	inline const FSlateBrush* ChipBrush()
	{
		static const FSlateRoundedBoxBrush B(Srgb(0x3a, 0x40, 0x4a), 9.f, FLinearColor::Transparent, 0.f);
		return &B;
	}
	inline const FSlateBrush* ThumbWellBrush()
	{
		static const FSlateRoundedBoxBrush B(Srgb(0x1a, 0x1c, 0x20), 6.f, FLinearColor::Transparent, 0.f);
		return &B;
	}

	inline FSlateFontInfo Font(int32 Size, bool bBold = false)
	{
		return FCoreStyle::GetDefaultFontStyle(bBold ? "Bold" : "Regular", Size);
	}
}
