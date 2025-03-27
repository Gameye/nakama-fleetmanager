# Nakama And Gameye Integration

Nakama Fleet Manager implementation for Gameye.

## Introduction

Similar to the already existing [AWS GameLift Integration](https://github.com/heroiclabs/nakama-gamelift), the `fleetmanager` package in this repository implements [Nakama](https://heroiclabs.com/nakama)'s Go runtime Fleet Manager interface to interact with [Gameye](https://docs.gameye.com/) managed environments.

This enables the use of Nakama's social gameplay and matchmaking features and Gameye's Session API which manages and orchestrates game sessions on-demand.

## Prerequisites

As of the time of this writing, Gameye is not yet a full self-servicing platform and requires users to speak to a Sales representative and technical support team to gain access. The following configurations are required by this integration:
- An API token
- An URL to one of Gameye's environments
- Gameye image name (Also known as a Gameye application)
- Gameye region (Also known as a Gameye location)

You must also have already published a game server build to one of the image registries Gameye pulls images from. The associated build version number is also required in the configuration.

## Installation

The fleet manager can be installed using `go get`:

```sh
go get github.com/Gameye/nakama-fleetmanager/fleetmanager
```

We recommend taking a look at any of the examples in the [examples](./examples) directory.

## Limitations

There are some limitations to this integration that the existing GameLift integration does not have. They are the following:
- Lack of Pagination support in Gameye's Session API. This prevents us from fully implementing the FleetManager#List querying mechanism.
- Gameye does not support latency based matchmaking. This means that this integration does not do anything with `runtime.FleetUserLatencies`.
- The GameLift integration relies on AWS SQS to queue in-flight requests for game sessions. Gameye currently does not support a Job based system. Sessions are spun up immediately as POST requests resolve. Retries are not implemented yet should Gameye run out of capacity.

## Usage

Just like with the GameLift integration, the `fleetmanager` instance has to be created within your Nakama plugin's `InitModule` function.

```go
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
```

### Matchmaking Events

```go
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
```

## Development

```sh
$ go version
go version go1.23.5 linux/amd64
```

```sh
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
```

```sh
docker compose build && docker compose up -d
```

### Downloading Dependencies

```sh
go get -v ./...
```

```sh
rm -rf vendor && go mod vendor
```

### Regenerating OpenAPI Code

```sh
oapi-codegen --config=api/openapi/client_config.yaml api/openapi/client.yaml
```