# Nakama And Gameye Integration

Nakama Fleet Manager implementation for Gameye.

## Introduction

Similar to the already existing [AWS GameLift Integration](https://github.com/heroiclabs/nakama-gamelift), the `fleetmanager` package in this repository implements [Nakama](https://heroiclabs.com/nakama)'s Go runtime Fleet Manager interface to interact with [Gameye](https://docs.gameye.com/) managed environments.

This enables the use of Nakama's social gameplay and matchmaking features and Gameye's Session API which manages and orchestrates game sessions on-demand.

## Prerequisites

As of the time of this writing, Gameye is not yet a full self-servicing platform and requires users to speak to a Sales representative and technical support team to gain access. The following configurations are required by this integration:
- An API token
- An URL to one of Gameye's environments
- Gameye image name
- Gameye region

You must also have already published a game server build to one of the image registries Gameye pulls images from. The associated build is also required in the configuration.

## Installation

The fleet manager can be installed using `go get`:

```sh
go get github.com/Gameye/nakama-fleetmanager/fleetmanager
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

## Regenerating OpenAPI Code

```sh
oapi-codegen --config=api/openapi/client_config.yaml api/openapi/client.yaml
```