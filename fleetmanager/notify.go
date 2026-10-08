package fleetmanager

import (
	"context"
	"errors"

	"github.com/heroiclabs/nakama-common/runtime"
)

const (
	// NotificationSubject is the subject of the notifications
	// NotifyConnectionInfo sends. Clients can match on it, for example when
	// listing persistent notifications after a reconnect.
	NotificationSubject = "gameye_match"

	// NotificationCode is the code of the notifications NotifyConnectionInfo
	// sends. Nakama reserves codes <= 0 for itself.
	NotificationCode = 7300

	// Keys of the notification content.
	NotificationKeyHost      = "host"
	NotificationKeyPort      = "port"
	NotificationKeySessionId = "session_id"
)

// NotifyConnectionInfo tells each user where to connect: it sends one
// notification per user with the instance's host, port and session id, plus
// whatever extras(userId) returns for that user (for example a per-player
// seat token). extras may be nil, and cannot override host, port or
// session_id. Persistent notifications survive a client reconnect.
//
// The notifications go out in one NotificationsSend call. Call it from the
// Create callback with a context that outlives the matchmaker hook.
func NotifyConnectionInfo(
	ctx context.Context,
	nk runtime.NakamaModule,
	userIds []string,
	instance *runtime.InstanceInfo,
	extras func(userId string) map[string]any,
	persistent bool,
) error {
	if instance == nil || instance.ConnectionInfo == nil {
		return errors.New("notify connection info: instance has no connection info")
	}
	if len(userIds) == 0 {
		return nil
	}

	notifications := make([]*runtime.NotificationSend, 0, len(userIds))
	for _, userId := range userIds {
		content := make(map[string]any)
		if extras != nil {
			for k, v := range extras(userId) {
				content[k] = v
			}
		}
		content[NotificationKeyHost] = instance.ConnectionInfo.IpAddress
		content[NotificationKeyPort] = instance.ConnectionInfo.Port
		content[NotificationKeySessionId] = instance.Id

		notifications = append(notifications, &runtime.NotificationSend{
			UserID:     userId,
			Subject:    NotificationSubject,
			Content:    content,
			Code:       NotificationCode,
			Persistent: persistent,
		})
	}

	return nk.NotificationsSend(ctx, notifications)
}
