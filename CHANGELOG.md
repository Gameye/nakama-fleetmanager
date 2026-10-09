# Changelog

All notable changes to this project are documented here. The project follows [Semantic Versioning](https://semver.org/); before v1.0.0, minor releases may break the API.

## v0.1.0

First tagged release. Requires Nakama 3.39.0 or later.

### Breaking changes

- **Nakama 3.39.0 or later.** The module requires nakama-common v1.46.0 and Go 1.26.3. Older Nakama releases have a different fleet manager interface and cannot load it.
- **`Create` returns the session id.** It now has the nakama-common v1.46 signature, `Create(...) (map[string]string, error)`, and returns `{"session_id": <id>}`. The same id is `InstanceInfo.Id` in the callback.
- **`Create` with user ids joins them.** The callback receives `InstanceInfo` and one `SessionInfo` per user, so callers no longer call `Join` after `Create`.
- **Ports are a map.** `gameye.Session` and `gameye.SessionListEntry` replace `Port int` with `Ports map[string]int`, keyed by `"<container port>/<protocol>"`. Use `gameye.HostPort` to pick one. The fleet manager picks the host port for `GameyeConfig.Port`, or the lowest exposed container port when it is empty.
- **Error sentinels.** API errors are `*gameye.ApiError` values that wrap `ErrUnauthorized` (401), `ErrQuotaExceeded` (402), `ErrForbidden` (403), `ErrNotFound` (404), `ErrNoCapacity` (420) or `ErrInternalServer` (5xx). `ErrRanOutOfCompute` is a deprecated alias of `ErrNoCapacity`.
- **`ApiError.Error()` text changed.** It used to return the bare message; it now reads `gameye api error <status>: <message>: <details>`. Match errors with `errors.Is` against the sentinels above, not on the message text.
- **`Delete` is idempotent.** `SessionStop`, and so `Delete`, returns nil when Gameye answers 404 (unknown session) or 409 (already stopped). Previously both returned an `*ApiError`, so callers that detected a missing session from `Delete`'s error no longer see one.
- **`Create` validates metadata.** It returns an error, without starting a session, for the metadata key `"env"` (pass container env under `"gameye.env"`), for a non-string `"gameye.external_id"`, and for a `"gameye.env"` value that isn't a map of non-empty strings. Previously every metadata key, these included, was sent as a label.
- **Generated client types renamed.** `pkg/api/generated/openapi/client` is regenerated: request and response bodies gain a `Body` suffix (`SessionRun` → `SessionRunBody`, `SessionRunOk` → `SessionRunOkBody`, `SessionListOk` → `SessionListOkBody`, `JoinSessionOk` → `JoinSessionOkBody`, `LeaveSessionOk` → `LeaveSessionOkBody`). This only affects code that imports the generated package directly; code that uses the `gameye` and `fleetmanager` packages is not affected by the rename.

### Added

- `Create` runs the Gameye call on a context detached from the matchmaker hook, which Nakama cancels when the hook returns, bounded by `GameyeConfig.CreateTimeout` (default 60s). On expiry the callback gets `runtime.CreateTimeout` and the session is stopped.
- An empty `GameyeConfig.BaseUrl` now means the self-serve Session API, `https://api.sandbox-gameye.gameye.net` (production contracts set `https://api.production-gameye.gameye.net`).
- `GameyeConfig.Ttl` (default `30m`), `Port`, `CreateTimeout`, `ReapInterval` and `Env`.
- Env pass-through: `metadata["gameye.env"]` (merged over `GameyeConfig.Env`) is sent as container env and never as labels, instance metadata or Nakama storage.
- `metadata["gameye.external_id"]` sets the session's external id.
- `NotifyConnectionInfo` sends each matched player a `gameye_match` notification with host, port, session id and optional per-player extras.
- A reaper that deletes stored instances for sessions Gameye no longer runs.
- The API client is regenerated from the current production Session API spec.
- Unit tests for every fleet manager method, and GitHub Actions CI that runs them and builds the plugin with Nakama 3.39.0 and 3.41.0.
- A rewritten example (`examples/main`) and Docker setup for Nakama 3.41.0.
