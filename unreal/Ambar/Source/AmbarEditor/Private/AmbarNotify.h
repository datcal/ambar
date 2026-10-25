#pragma once

#include "CoreMinimal.h"
#include "Framework/Notifications/NotificationManager.h"
#include "Widgets/Notifications/SNotificationList.h"

namespace AmbarUI
{
	/** A toast in the corner of the editor, green or red. */
	inline void Notify(const FString& Message, bool bOk)
	{
		FNotificationInfo Info(FText::FromString(Message));
		Info.ExpireDuration = bOk ? 4.f : 8.f;
		Info.bUseSuccessFailIcons = true;
		Info.bFireAndForget = true;
		const TSharedPtr<SNotificationItem> Item = FSlateNotificationManager::Get().AddNotification(Info);
		if (Item.IsValid())
		{
			Item->SetCompletionState(bOk ? SNotificationItem::CS_Success : SNotificationItem::CS_Fail);
		}
	}
}
