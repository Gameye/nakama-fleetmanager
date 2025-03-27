package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Gameye/nakama-fleetmanager/fleetmanager"
	"github.com/heroiclabs/nakama-common/runtime"
)

const (
	ENV_GAMEYE_URL           = "GAMEYE_API_URL"
	ENV_GAMEYE_TOKEN         = "GAMEYE_API_TOKEN"
	ENV_GAMEYE_IMAGE         = "GAMEYE_API_IMAGE"
	ENV_GAMEYE_IMAGE_VERSION = "GAMEYE_API_IMAGE_VERSION"
	ENV_GAMEYE_REGION        = "GAMEYE_API_REGION"
)

func InitModule(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer) error {
	start := time.Now()

	envValue := ctx.Value(runtime.RUNTIME_CTX_ENV)
	if envValue == nil {
		return fmt.Errorf("nil %v", runtime.RUNTIME_CTX_ENV)
	}

	env, ok := envValue.(map[string]string)
	if !ok {
		return runtime.NewError(fmt.Sprintf("unable to cast '%v' to a map", runtime.RUNTIME_CTX_ENV), 3)
	}

	url, exists := env[ENV_GAMEYE_URL]
	if !exists {
		return runtime.NewError(fmt.Sprintf("missing nakama runtime env var %v", ENV_GAMEYE_URL), 3)
	}

	token, exists := env[ENV_GAMEYE_TOKEN]
	if !exists {
		return runtime.NewError(fmt.Sprintf("missing nakama runtime env var %v", ENV_GAMEYE_TOKEN), 3)
	}

	image, exists := env[ENV_GAMEYE_IMAGE]
	if !exists {
		return runtime.NewError(fmt.Sprintf("missing nakama runtime env var %v", ENV_GAMEYE_IMAGE), 3)
	}

	region, exists := env[ENV_GAMEYE_REGION]
	if !exists {
		return runtime.NewError(fmt.Sprintf("missing nakama runtime env var %v", ENV_GAMEYE_REGION), 3)
	}

	version, exists := env[ENV_GAMEYE_IMAGE_VERSION]
	if !exists {
		return runtime.NewError(fmt.Sprintf("missing nakama runtime env var %v", ENV_GAMEYE_IMAGE_VERSION), 3)
	}

	config := fleetmanager.GameyeConfig{
		BaseUrl:  url,
		ApiToken: token,
		Image:    image,
		Version:  version,
		Region:   region,
	}

	fleetManager, err := fleetmanager.NewGameyeFleetManager(ctx, config, logger, db, initializer, nk)
	if err != nil {
		return err
	}

	if err = initializer.RegisterFleetManager(fleetManager); err != nil {
		return err
	}

	initializer.RegisterMatchmakerMatched(func(
		ctx context.Context,
		logger runtime.Logger,
		db *sql.DB,
		nk runtime.NakamaModule,
		entries []runtime.MatchmakerEntry,
	) (string, error) {
		var userIds []string
		for _, entry := range entries {
			userIds = append(userIds, entry.GetPresence().GetUserId())
		}

		onResult := func(
			status runtime.FmCreateStatus,
			instanceInfo *runtime.InstanceInfo,
			sessionInfo []*runtime.SessionInfo,
			metadata map[string]any, err error,
		) {
			switch status {
			case runtime.CreateSuccess:
				logger.Info("successfully started session %v", instanceInfo.Id)

				_, err := fleetManager.Join(ctx, instanceInfo.Id, userIds, make(map[string]string))
				if err != nil {
					logger.Error(err.Error(), "failed to make players join")
					return
				}

				logger.Info("successfully registered players %v to session %v", userIds, instanceInfo.Id)

			case runtime.CreateError:
				logger.Info("failed to start a session")
			}
		}

		err := fleetManager.Create(ctx, len(userIds), userIds, nil, make(map[string]any), onResult)
		if err != nil {
			logger.Error(err.Error())
		}

		return "", nil
	})

	logger.Info("Successfully registered the Gameye fleet manager which took %v", time.Since(start))

	return nil
}
