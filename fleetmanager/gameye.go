package fleetmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/Gameye/nakama-fleetmanager/gameye"
	gameyeApi "github.com/Gameye/nakama-fleetmanager/pkg/api/generated/openapi/client"
	"github.com/heroiclabs/nakama-common/runtime"
)

const (
	StorageGameyeInstancesCollection = "_gameye_instances"

	// CreateSessionIdKey is the key of the Gameye session id in the map that
	// Create returns. The same id is InstanceInfo.Id in the callback.
	CreateSessionIdKey = "session_id"

	// MetadataKeyExternalId is a reserved Create metadata key. A string value
	// is sent to Gameye as the session's external_id (for example the Nakama
	// match id) and is not sent as a label.
	MetadataKeyExternalId = "gameye.external_id"

	// MetadataKeyEnv is a reserved Create metadata key for environment
	// variables passed to the game server container: a map[string]string, or
	// a map[string]any whose values are all strings. They are merged over
	// GameyeConfig.Env, sent as the session's env, and never sent as labels,
	// returned as instance metadata or written to Nakama storage. Gameye
	// ignores env when a warm pool serves the session.
	MetadataKeyEnv = "gameye.env"

	// labelKeyEnv is the label in which Gameye may echo the container env. It
	// is stripped from instance metadata and rejected as a Create metadata key.
	labelKeyEnv = "env"

	// DefaultTtl is the session lifetime used when GameyeConfig.Ttl is empty.
	// Gameye force-stops a session when its TTL expires.
	DefaultTtl = "30m"

	// DefaultCreateTimeout bounds the Gameye session start made by Create.
	DefaultCreateTimeout = 60 * time.Second

	// DefaultReapInterval is how often the reaper runs when
	// GameyeConfig.ReapInterval is zero.
	DefaultReapInterval = 2 * time.Minute

	// cleanupTimeout bounds the best-effort stop of a session that Create
	// gave up on.
	cleanupTimeout = 15 * time.Second
)

var (
	ErrSecurityProvider = errors.New("error creating securityprovider")
	ErrCreateClient     = errors.New("error creating gameye sdk client")
	ErrNoBaseUrl        = errors.New("no base url provided")
	ErrNoApiToken       = errors.New("no api token provided")
	ErrNoRegion         = errors.New("no region provided")
	ErrNoImage          = errors.New("no image provided")
	ErrNoVersion        = errors.New("no image version provided")
	ErrInvalidPort      = errors.New(`invalid port: use "<container port>/<tcp|udp>", e.g. "7360/tcp"`)
	ErrInvalidTtl       = errors.New(`invalid ttl: use hours and/or minutes, e.g. "30m", "2h" or "1h30m"`)
	ErrInvalidTimeout   = errors.New("create timeout must not be negative")
	ErrInvalidEnv       = errors.New("invalid env: names and values must be non-empty strings")
)

var (
	portKeyPattern = regexp.MustCompile(`^[0-9]{1,5}/(tcp|udp)$`)
	ttlPattern     = regexp.MustCompile(`^([0-9]+h)?([0-9]+m)?$`)
)

type GameyeConfig struct {
	// BaseUrl of the Gameye Session API. Empty means
	// https://api.production-gameye.gameye.net (gameye.DefaultBaseUrl).
	BaseUrl  string
	ApiToken string
	Region   string
	Image    string
	Version  string

	// Ttl is the maximum lifetime of each session, in hours and/or minutes
	// ("30m", "2h", "1h30m"). Empty means DefaultTtl.
	Ttl string

	// Port is the container port, with protocol, whose host port players
	// connect to, e.g. "7360/tcp". Empty picks the lowest exposed container
	// port. Set it when the image exposes more than one port.
	Port string

	// CreateTimeout bounds the Gameye API call that Create makes in the
	// background. On expiry the callback receives runtime.CreateTimeout.
	// Zero means DefaultCreateTimeout.
	CreateTimeout time.Duration

	// ReapInterval is how often the fleet manager lists Gameye sessions and
	// deletes stored instances Gameye no longer has. Zero means
	// DefaultReapInterval; a negative value disables the reaper.
	ReapInterval time.Duration

	// Env is passed to every session's container. Create metadata under
	// MetadataKeyEnv is merged over it, winning on conflict.
	Env map[string]string
}

// withDefaults fills unset optional fields.
func (c GameyeConfig) withDefaults() GameyeConfig {
	if c.BaseUrl == "" {
		c.BaseUrl = gameye.DefaultBaseUrl
	}
	if c.Ttl == "" {
		c.Ttl = DefaultTtl
	}
	if c.CreateTimeout == 0 {
		c.CreateTimeout = DefaultCreateTimeout
	}
	if c.ReapInterval == 0 {
		c.ReapInterval = DefaultReapInterval
	}
	return c
}

type GameyeFleetManager struct {
	// ctx is the InitModule context; background work (the reaper) runs on it.
	ctx             context.Context
	config          GameyeConfig
	logger          runtime.Logger
	apiClient       gameye.ApiClient
	storage         InstanceStorage
	nk              runtime.NakamaModule
	callbackHandler runtime.FmCallbackHandler
	// reaperDone is closed when the reaper stops; nil when it never started.
	reaperDone chan struct{}
}

func (c GameyeConfig) Validate() error {
	var err []error

	if len(c.BaseUrl) == 0 {
		err = append(err, ErrNoBaseUrl)
	}

	if len(c.ApiToken) == 0 {
		err = append(err, ErrNoApiToken)
	}

	if len(c.Region) == 0 {
		err = append(err, ErrNoRegion)
	}

	if len(c.Image) == 0 {
		err = append(err, ErrNoImage)
	}

	if len(c.Version) == 0 {
		err = append(err, ErrNoVersion)
	}

	if c.Port != "" && !portKeyPattern.MatchString(c.Port) {
		err = append(err, fmt.Errorf("%w: %q", ErrInvalidPort, c.Port))
	}

	if c.Ttl != "" && !ttlPattern.MatchString(c.Ttl) {
		err = append(err, fmt.Errorf("%w: %q", ErrInvalidTtl, c.Ttl))
	}

	if c.CreateTimeout < 0 {
		err = append(err, ErrInvalidTimeout)
	}

	if envErr := validateEnv(c.Env); envErr != nil {
		err = append(err, envErr)
	}

	return errors.Join(err...)
}

// NewGameyeFleetManager validates config and returns the fleet manager. Pass
// the InitModule context as ctx: the reaper started by Init runs on it and
// stops when it ends.
func NewGameyeFleetManager(
	ctx context.Context,
	config GameyeConfig,
	logger runtime.Logger,
	db *sql.DB,
	initializer runtime.Initializer,
	nk runtime.NakamaModule,
) (*GameyeFleetManager, error) {
	config = config.withDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}

	apiClient, err := gameye.NewApiClient(config.BaseUrl, config.ApiToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCreateClient, err)
	}

	if ctx == nil {
		ctx = context.Background()
	}

	g := &GameyeFleetManager{
		ctx:       ctx,
		config:    config,
		logger:    logger,
		apiClient: apiClient,
	}

	return g, nil
}

func (fm *GameyeFleetManager) Init(
	nk runtime.NakamaModule,
	callbackHandler runtime.FmCallbackHandler,
) error {
	fm.nk = nk
	fm.callbackHandler = callbackHandler
	if fm.storage == nil {
		fm.storage = NewNakamaStorage(nk)
	}
	fm.startReaper()
	return nil
}

// Create starts a Gameye session and returns {CreateSessionIdKey: id}
// immediately. The API call runs in the background on a context detached from
// ctx (Nakama cancels the MatchmakerMatched hook's context as soon as the hook
// returns), bounded by GameyeConfig.CreateTimeout. The callback then receives
// CreateSuccess with the instance, CreateTimeout, or CreateError.
//
// With userIds, Create also joins those users to the session before the
// callback, which then receives one SessionInfo per user; if the join fails
// the session is stopped and the callback receives CreateError. The instance's
// Metadata holds the non-reserved metadata keys. Invalid metadata (see
// MetadataKeyExternalId, MetadataKeyEnv) returns an error before any API call.
func (fm *GameyeFleetManager) Create(
	ctx context.Context,
	maxPlayers int,
	userIds []string,
	latencies []runtime.FleetUserLatencies,
	metadata map[string]any,
	callback runtime.FmCreateCallbackFn,
) (map[string]string, error) {
	requestBody, instanceMetadata, err := fm.sessionRequest(metadata)
	if err != nil {
		return nil, err
	}

	id := fm.callbackHandler.GenerateCallbackId()
	requestBody.ID = id

	if callback != nil {
		fm.callbackHandler.SetCallback(id, callback)
	}

	go fm.startSession(context.WithoutCancel(ctx), requestBody, userIds, instanceMetadata)

	return map[string]string{CreateSessionIdKey: id}, nil
}

// sessionRequest splits Create metadata into the session start request and
// the instance metadata. Reserved keys (MetadataKeyExternalId, MetadataKeyEnv)
// go to the request only; every other key becomes a label and instance
// metadata. Env never reaches labels or the returned metadata.
func (fm *GameyeFleetManager) sessionRequest(metadata map[string]any) (gameye.SessionRun, map[string]any, error) {
	var externalId string
	var metadataEnv map[string]string
	labels := make(map[string]string)
	var instanceMetadata map[string]any

	for key, rawValue := range metadata {
		switch key {
		case MetadataKeyExternalId:
			value, ok := rawValue.(string)
			if !ok {
				return gameye.SessionRun{}, nil, fmt.Errorf("metadata %q must be a string, got %T", MetadataKeyExternalId, rawValue)
			}
			externalId = value

		case MetadataKeyEnv:
			env, err := parseEnv(rawValue)
			if err != nil {
				return gameye.SessionRun{}, nil, fmt.Errorf("metadata %q: %w", MetadataKeyEnv, err)
			}
			metadataEnv = env

		case labelKeyEnv:
			return gameye.SessionRun{}, nil, fmt.Errorf("metadata key %q is reserved; pass container env under %q", labelKeyEnv, MetadataKeyEnv)

		default:
			label, err := labelValue(rawValue)
			if err != nil {
				return gameye.SessionRun{}, nil, fmt.Errorf("error marshalling metadata %q: %v", key, err)
			}
			labels[key] = label
			if instanceMetadata == nil {
				instanceMetadata = make(map[string]any)
			}
			instanceMetadata[key] = rawValue
		}
	}

	return gameye.SessionRun{
		Region:     fm.config.Region,
		Image:      fm.config.Image,
		Tag:        fm.config.Version,
		Labels:     labels,
		EnvVars:    mergeEnv(fm.config.Env, metadataEnv),
		Ttl:        fm.config.Ttl,
		ExternalID: externalId,
	}, instanceMetadata, nil
}

func labelValue(rawValue any) (string, error) {
	switch value := rawValue.(type) {
	case uint8, uint16, uint32, uint64, int8, int16, int32, int64, int, string, bool, float64, float32:
		return fmt.Sprint(value), nil
	default:
		bytes, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return string(bytes), nil
	}
}

// parseEnv accepts a map[string]string, or a map[string]any whose values are
// all strings. Errors name keys and types only, never values.
func parseEnv(raw any) (map[string]string, error) {
	var env map[string]string
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case map[string]string:
		env = v
	case map[string]any:
		env = make(map[string]string, len(v))
		for key, value := range v {
			s, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%w: value of %q must be a string, got %T", ErrInvalidEnv, key, value)
			}
			env[key] = s
		}
	default:
		return nil, fmt.Errorf("%w: must be a map[string]string, got %T", ErrInvalidEnv, raw)
	}

	if err := validateEnv(env); err != nil {
		return nil, err
	}
	return env, nil
}

// validateEnv rejects what Gameye's API rejects: empty names and empty values.
func validateEnv(env map[string]string) error {
	for key, value := range env {
		if key == "" {
			return fmt.Errorf("%w: empty variable name", ErrInvalidEnv)
		}
		if value == "" {
			return fmt.Errorf("%w: %q has an empty value", ErrInvalidEnv, key)
		}
	}
	return nil
}

// mergeEnv returns a new map of base overlaid with override; nil when empty.
func mergeEnv(base, override map[string]string) map[string]string {
	if len(base)+len(override) == 0 {
		return nil
	}
	env := make(map[string]string, len(base)+len(override))
	for k, v := range base {
		env[k] = v
	}
	for k, v := range override {
		env[k] = v
	}
	return env
}

// metadataFromLabels converts session labels to instance metadata, dropping
// the "env" label in which Gameye may echo the container env.
func metadataFromLabels(labels map[string]string) map[string]any {
	var metadata map[string]any
	for key, value := range labels {
		if key == labelKeyEnv {
			continue
		}
		if metadata == nil {
			metadata = make(map[string]any, len(labels))
		}
		metadata[key] = value
	}
	return metadata
}

func (fm *GameyeFleetManager) startSession(ctx context.Context, req gameye.SessionRun, userIds []string, metadata map[string]any) {
	id := req.ID
	fail := func(status runtime.FmCreateStatus, err error) {
		fm.callbackHandler.InvokeCallback(id, status, nil, nil, nil, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, fm.config.CreateTimeout)
	started, err := fm.apiClient.SessionRun(runCtx, req)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	cancel()

	if err != nil {
		if timedOut || errors.Is(err, context.DeadlineExceeded) {
			// Gameye may have started the session after all; stop it so it
			// doesn't run until its TTL.
			fm.stopSession(ctx, id)
			fail(runtime.CreateTimeout, fmt.Errorf("starting gameye session %v timed out after %v: %w", id, fm.config.CreateTimeout, err))
			return
		}

		fail(runtime.CreateError, fmt.Errorf("error starting gameye session %v: %w", id, err))
		return
	}

	port, ok := gameye.HostPort(started.PortMap(), fm.config.Port)
	if !ok {
		fm.stopSession(ctx, id)
		fail(runtime.CreateError, fmt.Errorf("gameye session %v exposes no port matching %q (ports: %v)", id, fm.config.Port, started.PortMap()))
		return
	}

	var sessionInfo []*runtime.SessionInfo
	playerCount := 0
	if len(userIds) > 0 {
		joinCtx, cancel := context.WithTimeout(ctx, fm.config.CreateTimeout)
		players, err := fm.apiClient.SessionJoin(joinCtx, gameye.SessionJoin{ID: id, PlayerIDs: userIds})
		cancel()
		if err != nil {
			fm.stopSession(ctx, id)
			fail(runtime.CreateError, fmt.Errorf("error joining users to gameye session %v: %w", id, err))
			return
		}

		sessionInfo = make([]*runtime.SessionInfo, 0, len(userIds))
		for _, userId := range userIds {
			sessionInfo = append(sessionInfo, &runtime.SessionInfo{UserId: userId, SessionId: id})
		}
		playerCount = max(len(players), len(userIds))
	}

	// The id we requested is authoritative; the response id is optional.
	instanceInfo := &runtime.InstanceInfo{
		Id:          id,
		CreateTime:  time.Now(),
		PlayerCount: playerCount,
		Status:      string(gameyeApi.Running),
		ConnectionInfo: &runtime.ConnectionInfo{
			IpAddress: started.Host,
			Port:      port,
		},
		Metadata: metadata,
	}

	// Store before invoking the callback, so a Join from the callback finds it.
	storeCtx, cancel := context.WithTimeout(ctx, fm.config.CreateTimeout)
	if err := fm.storage.Write(storeCtx, []*runtime.InstanceInfo{instanceInfo}); err != nil {
		fm.logger.Error("error writing gameye session %v to nakama storage: %v", id, err)
	}
	cancel()

	fm.callbackHandler.InvokeCallback(id, runtime.CreateSuccess, instanceInfo, sessionInfo, nil, nil)
}

// stopSession stops a session best-effort; failures are logged.
func (fm *GameyeFleetManager) stopSession(ctx context.Context, id string) {
	stopCtx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	if err := fm.apiClient.SessionStop(stopCtx, gameye.SessionStop{ID: id}); err != nil {
		fm.logger.Error("error stopping gameye session %v: %v", id, err)
	}
}

func (fm *GameyeFleetManager) Delete(
	ctx context.Context,
	id string,
) error {
	if err := fm.apiClient.SessionStop(ctx, gameye.SessionStop{ID: id}); err != nil {
		return err
	}

	return fm.storage.Delete(ctx, []string{id})
}

func (fm *GameyeFleetManager) Get(
	ctx context.Context,
	id string,
) (instance *runtime.InstanceInfo, err error) {
	session, err := fm.apiClient.SessionDescribe(ctx, gameye.SessionDescribe{ID: id})
	if err != nil {
		if errors.Is(err, gameye.ErrNotFound) {
			// Gameye no longer has the session; drop our record of it.
			if delErr := fm.storage.Delete(ctx, []string{id}); delErr != nil {
				fm.logger.Error("error deleting gameye session %v from nakama storage: %v", id, delErr)
			}
		}
		return nil, fmt.Errorf("error describing session %v: %w", id, err)
	}

	connectionInfo := &runtime.ConnectionInfo{
		IpAddress: session.IPV4Address,
		Port:      fm.hostPort(session.ID, session.Ports),
	}

	instance = &runtime.InstanceInfo{
		Id:             session.ID,
		CreateTime:     session.Created,
		PlayerCount:    session.PlayerCount,
		Status:         string(session.Status),
		ConnectionInfo: connectionInfo,
		Metadata:       metadataFromLabels(session.Labels),
	}

	if isLive(string(session.Status)) {
		if err = fm.storage.Write(ctx, []*runtime.InstanceInfo{instance}); err != nil {
			return nil, err
		}

	} else if err = fm.storage.Delete(ctx, []string{session.ID}); err != nil {
		return nil, err
	}

	return instance, nil
}

func (fm *GameyeFleetManager) List(
	ctx context.Context,
	query string,
	limit int,
	previousCursor string,
) (list []*runtime.InstanceInfo, nextCursor string, err error) {
	params := gameye.SessionList{
		Region: fm.config.Region,
		Image:  fm.config.Image,
		Tag:    fm.config.Version,
	}

	response, err := fm.apiClient.SessionList(ctx, params)
	if err != nil {
		return list, nextCursor, fmt.Errorf("error listing sessions: %w", err)
	}

	var instances, live []*runtime.InstanceInfo
	var ended []string
	for _, session := range response {
		connectionInfo := &runtime.ConnectionInfo{
			IpAddress: session.IPV4Address,
			Port:      fm.hostPort(session.ID, session.Ports),
		}

		instance := &runtime.InstanceInfo{
			Id:             session.ID,
			CreateTime:     session.Created,
			PlayerCount:    session.PlayerCount,
			Status:         string(session.Status),
			ConnectionInfo: connectionInfo,
			Metadata:       metadataFromLabels(session.Labels),
		}

		instances = append(instances, instance)
		if isLive(session.Status) {
			live = append(live, instance)
		} else {
			ended = append(ended, session.ID)
		}
	}

	if err = fm.storage.Write(ctx, live); err != nil {
		return instances, nextCursor, err
	}
	if err = fm.storage.Delete(ctx, ended); err != nil {
		return instances, nextCursor, err
	}

	return instances, nextCursor, nil
}

func (fm *GameyeFleetManager) Join(
	ctx context.Context,
	id string,
	userIds []string,
	metadata map[string]string,
) (joinInfo *runtime.JoinInfo, err error) {
	instanceInfo, err := fm.storage.Read(ctx, id)
	if err != nil {
		return nil, err
	}

	if instanceInfo == nil {
		instanceInfo, err = fm.Get(ctx, id)
		if err != nil {
			return nil, err
		}
	}

	players, err := fm.apiClient.SessionJoin(ctx, gameye.SessionJoin{
		PlayerIDs: userIds,
		ID:        instanceInfo.Id,
	})

	if err != nil {
		return nil, fmt.Errorf("error joining session %v: %w", id, err)
	}

	var sessionInfo []*runtime.SessionInfo
	for _, playerId := range players {
		sessionInfo = append(sessionInfo, &runtime.SessionInfo{
			UserId:    playerId,
			SessionId: id,
		})
	}

	instanceInfo.PlayerCount = len(players)
	if err = fm.storage.Write(ctx, []*runtime.InstanceInfo{instanceInfo}); err != nil {
		return nil, err
	}

	joinInfo = &runtime.JoinInfo{
		InstanceInfo: instanceInfo,
		SessionInfo:  sessionInfo,
	}

	return joinInfo, nil
}

func (fm *GameyeFleetManager) Update(
	ctx context.Context,
	id string,
	playerCount int,
	metadata map[string]any,
) error {
	instanceInfo, err := fm.storage.Read(ctx, id)
	if err != nil {
		return err
	}

	if instanceInfo == nil {
		_, err = fm.Get(ctx, id)
		return err
	}

	instanceInfo.PlayerCount = playerCount
	err = fm.storage.Write(ctx, []*runtime.InstanceInfo{instanceInfo})
	if err != nil {
		return err
	}

	return nil
}

// isLive reports whether a session in this status can still host players.
// Live sessions are stored; ended ones are removed from storage.
func isLive(status string) bool {
	switch gameyeApi.SessionStatus(status) {
	case gameyeApi.Running, gameyeApi.Draining, gameyeApi.Shuttingdown:
		return true
	default:
		return false
	}
}

// hostPort selects the configured port; 0 when the session doesn't expose it.
func (fm *GameyeFleetManager) hostPort(id string, ports map[string]int) int {
	port, ok := gameye.HostPort(ports, fm.config.Port)
	if !ok {
		fm.logger.Warn("gameye session %v exposes no port matching %q (ports: %v)", id, fm.config.Port, ports)
	}
	return port
}
