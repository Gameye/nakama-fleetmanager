// Command main is an example Nakama plugin that starts a Gameye session for
// every matchmaker match and tells the matched players where to connect.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Gameye/nakama-fleetmanager/fleetmanager"
	"github.com/Gameye/nakama-fleetmanager/gameye"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Nakama runtime.env keys read by InitModule. TTL and PORT are optional; URL
// defaults to Gameye's production Session API.
const (
	envUrl          = "GAMEYE_API_URL"
	envToken        = "GAMEYE_API_TOKEN"
	envImage        = "GAMEYE_API_IMAGE"
	envImageVersion = "GAMEYE_API_IMAGE_VERSION"
	envRegion       = "GAMEYE_API_REGION"
	envTtl          = "GAMEYE_API_TTL"
	envPort         = "GAMEYE_API_PORT"
)

const (
	// failedSubject is the subject of the notification players get when no
	// server could be started for their match.
	failedSubject = "gameye_failed"
	failedCode    = fleetmanager.NotificationCode + 1

	// notifyTimeout bounds the notification sent from the Create callback.
	notifyTimeout = 10 * time.Second
)

func InitModule(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer) error {
	start := time.Now()

	env, ok := ctx.Value(runtime.RUNTIME_CTX_ENV).(map[string]string)
	if !ok {
		return runtime.NewError("runtime env is missing", 3)
	}
	for _, key := range []string{envToken, envImage, envImageVersion, envRegion} {
		if env[key] == "" {
			return runtime.NewError(fmt.Sprintf("missing runtime env var %v", key), 3)
		}
	}

	fm, err := fleetmanager.NewGameyeFleetManager(ctx, fleetmanager.GameyeConfig{
		BaseUrl:  env[envUrl],
		ApiToken: env[envToken],
		Image:    env[envImage],
		Version:  env[envImageVersion],
		Region:   env[envRegion],
		Ttl:      env[envTtl],
		Port:     env[envPort],
	}, logger, db, initializer, nk)
	if err != nil {
		return err
	}
	if err := initializer.RegisterFleetManager(fm); err != nil {
		return err
	}

	if err := initializer.RegisterMatchmakerMatched(func(
		ctx context.Context,
		_ runtime.Logger,
		_ *sql.DB,
		nk runtime.NakamaModule,
		entries []runtime.MatchmakerEntry,
	) (string, error) {
		userIds := make([]string, 0, len(entries))
		for _, entry := range entries {
			userIds = append(userIds, entry.GetPresence().GetUserId())
		}

		// The callback runs after this hook has returned, when Nakama has
		// already cancelled ctx. Never use ctx in it.
		callback := func(status runtime.FmCreateStatus, instance *runtime.InstanceInfo, _ []*runtime.SessionInfo, _ map[string]any, err error) {
			notifyCtx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
			defer cancel()

			if status == runtime.CreateSuccess {
				// Create has already joined the players to the session.
				// Persistent, so a client that reconnects can still find it.
				if err := fleetmanager.NotifyConnectionInfo(notifyCtx, nk, userIds, instance, nil, true); err != nil {
					logger.Error("notifying players of session %v: %v", instance.Id, err)
				}
				return
			}

			reason := failureReason(status, err)
			logger.Warn("no game server for players %v (%v): %v", userIds, reason, err)
			if err := notifyFailed(notifyCtx, nk, userIds, reason); err != nil {
				logger.Error("notifying players of the failed session: %v", err)
			}
		}

		metadata := map[string]any{
			// Shows up as the session's external id in Gameye, so the
			// session can be traced back to the matchmaker ticket.
			fleetmanager.MetadataKeyExternalId: entries[0].GetTicket(),
			// Passed to the game server container as environment variables.
			fleetmanager.MetadataKeyEnv: map[string]string{
				"NAKAMA_MATCH_USER_IDS": strings.Join(userIds, ","),
			},
		}

		result, err := fm.Create(ctx, len(userIds), userIds, nil, metadata, callback)
		if err != nil {
			return "", err
		}
		logger.Info("starting gameye session %v for players %v", result[fleetmanager.CreateSessionIdKey], userIds)

		// Players connect to the Gameye server, not to a Nakama match.
		return "", nil
	}); err != nil {
		return err
	}

	logger.Info("Successfully registered the Gameye fleet manager which took %v", time.Since(start))
	return nil
}

// failureReason turns a Create failure into a short reason a client can act on.
func failureReason(status runtime.FmCreateStatus, err error) string {
	switch {
	case status == runtime.CreateTimeout:
		return "timeout"
	case errors.Is(err, gameye.ErrNoCapacity):
		return "no_capacity"
	case errors.Is(err, gameye.ErrQuotaExceeded):
		return "quota_exceeded"
	case errors.Is(err, gameye.ErrUnauthorized), errors.Is(err, gameye.ErrForbidden), errors.Is(err, gameye.ErrNotFound):
		return "misconfigured"
	case gameye.IsRetryable(err):
		return "unavailable"
	default:
		return "error"
	}
}

func notifyFailed(ctx context.Context, nk runtime.NakamaModule, userIds []string, reason string) error {
	notifications := make([]*runtime.NotificationSend, 0, len(userIds))
	for _, userId := range userIds {
		notifications = append(notifications, &runtime.NotificationSend{
			UserID:  userId,
			Subject: failedSubject,
			Content: map[string]any{"reason": reason},
			Code:    failedCode,
		})
	}
	return nk.NotificationsSend(ctx, notifications)
}
