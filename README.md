# Nakama Fleet Manager for Gameye

[![CI](https://github.com/Gameye/nakama-fleetmanager/actions/workflows/ci.yml/badge.svg)](https://github.com/Gameye/nakama-fleetmanager/actions/workflows/ci.yml)

The `fleetmanager` package implements [Nakama](https://heroiclabs.com/nakama)'s Go runtime Fleet Manager interface on top of [Gameye](https://gameye.com)'s Session API. When Nakama's matchmaker forms a match, the fleet manager starts a dedicated game server on Gameye, registers the matched players with it, and hands you the server's address to send to those players.

It follows the same pattern as Heroic Labs' [GameLift integration](https://github.com/heroiclabs/nakama-gamelift). For a complete game built on it, see [Gameye/scrapyard-nakama](https://github.com/Gameye/scrapyard-nakama) (a complete example, in progress).

## Compatibility

| Package | Nakama | nakama-common |
|---|---|---|
| v0.1.0 | 3.39.0 or later | v1.46.0 or later |

The package's `go.mod` requires nakama-common v1.46.0, the release that introduced the current `Create` signature (Nakama 3.39.0). The example in [`examples/main`](./examples/main) and the Docker files are pinned to Nakama 3.41.0 (nakama-common v1.48.0).

A Nakama Go plugin must be built exactly like the server that loads it:

- Build with the `heroiclabs/nakama-pluginbuilder` image whose tag matches your `heroiclabs/nakama` image.
- Require the nakama-common version your Nakama release ships with (v1.46.0 for 3.39, v1.48.0 for 3.41), and match every other dependency the server also uses. Look the versions up in Nakama's `vendor/modules.txt`. For this package that means nakama-common and `google.golang.org/protobuf` (v1.36.12 for Nakama 3.41).

A mismatch fails at startup with errors such as `plugin was built with a different version of package ...`. See Heroic Labs' [Go dependencies guide](https://heroiclabs.com/docs/nakama/server-framework/go-runtime/go-dependencies/).

## Prerequisites

- **A Gameye account.** Sign up at [trial.gameye.com](https://trial.gameye.com/).
- **An API token** with the scopes `session:start`, `session:read` and `session:stop`. The fleet manager starts sessions, lists and describes them (for `List`, `Get` and the reaper), and stops them (for `Delete` and to clean up sessions it gave up on).
- **An application** in Gameye with your game server image pushed to it, and the image tag you want to run.
- **The container port** your game server listens on, for example `7777/udp`. You need it when the image exposes more than one port.
- **No warm pool** for that application and region. Gameye serves warm-pool sessions from containers that were started in advance, so they ignore the env you pass to `Create`. See [Passing env to the game server](#passing-env-to-the-game-server).

## Installation

```sh
go get github.com/Gameye/nakama-fleetmanager@v0.1.0
```

Then pin nakama-common to the version your Nakama release uses (see [Compatibility](#compatibility)) and vendor:

```sh
go get github.com/heroiclabs/nakama-common@v1.48.0   # Nakama 3.41
go mod vendor
```

## Configuration

`NewGameyeFleetManager` takes a `fleetmanager.GameyeConfig`. The example reads it from Nakama's `runtime.env`, using the variable names below.

| Field | `runtime.env` key (example) | Required | Default | Description |
|---|---|---|---|---|
| `BaseUrl` | `GAMEYE_API_URL` | No | `https://api.sandbox-gameye.gameye.net` | Session API base URL. The default is Gameye's self-serve platform, where trial accounts live. Teams on a dedicated production contract use `https://api.production-gameye.gameye.net`. |
| `ApiToken` | `GAMEYE_API_TOKEN` | Yes | | API token with the scopes listed above. |
| `Image` | `GAMEYE_API_IMAGE` | Yes | | Gameye application name. |
| `Version` | `GAMEYE_API_IMAGE_VERSION` | Yes | | Image tag to run. |
| `Region` | `GAMEYE_API_REGION` | Yes | | Region to start sessions in. |
| `Ttl` | `GAMEYE_API_TTL` | No | `30m` | Maximum session lifetime, in hours and/or minutes (`30m`, `2h`, `1h30m`). Gameye stops the session when it expires. |
| `Port` | `GAMEYE_API_PORT` | No | lowest exposed port | Container port players connect to, as `<port>/<tcp\|udp>`. Set it when the image exposes more than one port. |
| `CreateTimeout` | | No | `60s` | Upper bound on starting a session. On expiry the callback gets `runtime.CreateTimeout`. |
| `ReapInterval` | | No | `2m` | How often the reaper runs. A negative value turns it off. |
| `Env` | | No | | Environment variables passed to every session. |

`NewGameyeFleetManager` validates the config and returns every problem at once.

## Usage

### Registration

Create and register the fleet manager in your plugin's `InitModule`. Pass the `InitModule` context: the reaper runs on it.

```go
fm, err := fleetmanager.NewGameyeFleetManager(ctx, fleetmanager.GameyeConfig{
	ApiToken: env["GAMEYE_API_TOKEN"],
	Image:    env["GAMEYE_API_IMAGE"],
	Version:  env["GAMEYE_API_IMAGE_VERSION"],
	Region:   env["GAMEYE_API_REGION"],
	Port:     env["GAMEYE_API_PORT"],
}, logger, db, initializer, nk)
if err != nil {
	return err
}
if err := initializer.RegisterFleetManager(fm); err != nil {
	return err
}
```

Elsewhere in your plugin you can also reach it through `nk.GetFleetManager()`.

### Starting a server for a matchmaker match

Three steps: call `Create` from the `MatchmakerMatched` hook, wait for the callback, and send the players the address with `NotifyConnectionInfo`. [`examples/main/main.go`](./examples/main/main.go) is the full version.

**1. Create.** Pass the matched user ids. `Create` returns at once with `{"session_id": <id>}` and starts the session in the background.

```go
initializer.RegisterMatchmakerMatched(func(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, entries []runtime.MatchmakerEntry) (string, error) {
	userIds := make([]string, 0, len(entries))
	for _, e := range entries {
		userIds = append(userIds, e.GetPresence().GetUserId())
	}

	metadata := map[string]any{
		fleetmanager.MetadataKeyExternalId: entries[0].GetTicket(),
		fleetmanager.MetadataKeyEnv:        map[string]string{"NAKAMA_MATCH_USER_IDS": strings.Join(userIds, ",")},
	}

	if _, err := fm.Create(ctx, len(userIds), userIds, nil, metadata, callback(userIds)); err != nil {
		return "", err
	}
	return "", nil
})
```

**2. Handle the callback.** It runs after the hook has returned. Nakama cancels the hook's `ctx` when the hook returns, so never use it in the callback; start a new context instead. On success the players are already joined to the session (`Create` calls Gameye's join for you), so don't call `Join` again.

**3. Notify the players.**

```go
func callback(userIds []string) runtime.FmCreateCallbackFn {
	return func(status runtime.FmCreateStatus, instance *runtime.InstanceInfo, _ []*runtime.SessionInfo, _ map[string]any, err error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if status != runtime.CreateSuccess {
			// Tell the players; see Errors below.
			return
		}
		_ = fleetmanager.NotifyConnectionInfo(ctx, nk, userIds, instance, nil, true)
	}
}
```

`NotifyConnectionInfo` sends each user one notification with subject `gameye_match` (`fleetmanager.NotificationSubject`) and code 7300. Its content holds `host`, `port` and `session_id`, plus whatever the `extras` function returns for that user, such as a per-player join token. Pass `persistent: true` so a client that reconnects can still list the notification.

On the client, listen for that subject and connect to the game server. With nakama-js:

```js
socket.onnotification = (n) => {
  if (n.subject === "gameye_match") {
    connectToGameServer(n.content.host, n.content.port);
  }
};
```

### Metadata

`Create` metadata keys become Gameye session labels and `InstanceInfo.Metadata`, except two reserved keys:

| Key | Value | Effect |
|---|---|---|
| `gameye.external_id` (`MetadataKeyExternalId`) | `string` | Sent as the session's external id, for tracing a session back to your own ids. |
| `gameye.env` (`MetadataKeyEnv`) | `map[string]string` | Passed to the container as env. See below. |

The key `env` is rejected.

### Passing env to the game server

Env comes from `GameyeConfig.Env` (every session) and `metadata["gameye.env"]` (one session), with the metadata winning on conflict. The package never sends env as labels, never returns it in `InstanceInfo.Metadata` and never writes it to Nakama storage.

Two caveats:

- **Warm pools ignore env.** A session served from a warm pool runs in a container that started before your request, so it never sees the env. Turn off the warm pool for the application and region if your server depends on per-match env.
- **Gameye may echo env back.** Gameye can return container env in the session's labels when sessions are read. The package strips it, but treat env as visible to anyone with `session:read` on your organisation. Pass per-match, short-lived values (a match token that expires) rather than long-lived secrets.

### Errors

Errors from the Session API wrap these sentinels in the `gameye` package, so use `errors.Is`:

| Error | HTTP | Meaning |
|---|---|---|
| `gameye.ErrUnauthorized` | 401 | Token missing, invalid or expired. |
| `gameye.ErrQuotaExceeded` | 402 | Your organisation's session quota is used up. |
| `gameye.ErrForbidden` | 403 | The token lacks a required scope. |
| `gameye.ErrNotFound` | 404 | Unknown session, region, application or tag. |
| `gameye.ErrNoCapacity` | 420 | No capacity in the region right now. |
| `gameye.ErrInternalServer` | 5xx | Gameye error; `gameye.IsRetryable(err)` is true. |

In the `Create` callback, `status` is `runtime.CreateTimeout` when the start took longer than `CreateTimeout`, and `runtime.CreateError` otherwise. When the package gives up on a session (timeout, no matching port, failed join) it stops it, so it does not run until its TTL. The example maps these to a `reason` and sends players a `gameye_failed` notification.

Config problems are reported by `NewGameyeFleetManager` (`ErrNoApiToken`, `ErrInvalidPort`, `ErrInvalidTtl` and so on), and bad metadata by `Create` before any API call.

### The reaper

The fleet manager stores each running session in the Nakama storage collection `_gameye_instances`. A background reaper, started in `Init`, lists your Gameye sessions every `ReapInterval` and deletes stored instances that Gameye no longer runs. It stops when the `InitModule` context ends.

## Limitations

- **No pagination or queries in `List`.** The Session API has no pagination, so `List` ignores the query, limit and cursor and returns every session for the configured region, application and tag.
- **No latency-based placement.** `Create` ignores `runtime.FleetUserLatencies` and always starts in the configured region.
- **No retry queue.** Sessions start as soon as `Create` is called. If Gameye has no capacity or your quota is used up, the callback gets the error and retrying is up to you.
- **No backfill helper.** `Join` registers more players with an existing session, but finding a session with free seats and sending those players to it is up to you.

## Run locally with Docker

[`compose.yml`](./compose.yml) builds [`examples/main`](./examples/main) with the Nakama 3.41.0 plugin builder and runs it with PostgreSQL 16.

1. Copy [`.env.example`](./.env.example) to `.env` and set `GAMEYE_API_TOKEN`. `.env` is gitignored, and compose passes the token to Nakama at run time, so it stays out of git and the image. Put your application, tag and region into [`examples/main/local.yml`](./examples/main/local.yml).
2. Build and start:

   ```sh
   docker compose up --build
   ```

3. Look for `Successfully registered the Gameye fleet manager` and `Startup done` in the Nakama logs. The console is at http://localhost:7351 (admin / password).

Stop and remove everything, including the database, with `docker compose down -v`.

## Development

Requires Go 1.26.3 or later.

Run the tests:

```sh
go vet ./...
go test -race -count=1 ./...
```

Build the example plugin the way CI does:

```sh
docker run --rm -v "$PWD":/src -w /src/examples/main --entrypoint sh heroiclabs/nakama-pluginbuilder:3.41.0 \
  -c 'go mod vendor && go build --trimpath --mod=vendor --buildmode=plugin -o backend.so .'
```

Regenerate the API client after changing [`api/openapi/client.yaml`](./api/openapi/client.yaml):

```sh
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1
oapi-codegen --config=api/openapi/client_config.yaml api/openapi/client.yaml
go mod tidy && go mod vendor
```

CI checks that `go.mod`, `go.sum` and `vendor/` are up to date, and builds the plugin with Nakama 3.39.0 and 3.41.0.

See [CHANGELOG.md](./CHANGELOG.md) for changes between releases.

## About Gameye

[Gameye](https://gameye.com/?utm_source=github&utm_medium=readme&utm_campaign=nakama-fleetmanager) is a managed hosting platform for multiplayer game servers. You bring a container image, and Gameye starts a dedicated server for each match through one API.

- Nakama guide: [gameye.com/docs/guides/integrations/nakama](https://gameye.com/docs/guides/integrations/nakama/?utm_source=github&utm_medium=readme&utm_campaign=nakama-fleetmanager)
- Gameye docs: [gameye.com/docs](https://gameye.com/docs/?utm_source=github&utm_medium=readme&utm_campaign=nakama-fleetmanager)
- Try it free: [trial.gameye.com](https://trial.gameye.com/?utm_source=github&utm_medium=readme&utm_campaign=nakama-fleetmanager)
