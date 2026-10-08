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

	// DefaultTtl is the session lifetime used when GameyeConfig.Ttl is empty.
	// Gameye force-stops a session when its TTL expires.
	DefaultTtl = "30m"

	// DefaultCreateTimeout bounds the Gameye session start made by Create.
	DefaultCreateTimeout = 60 * time.Second

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
	return c
}

type GameyeFleetManager struct {
	config          GameyeConfig
	logger          runtime.Logger
	apiClient       gameye.ApiClient
	nk              runtime.NakamaModule
	callbackHandler runtime.FmCallbackHandler
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

	return errors.Join(err...)
}

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

	g := &GameyeFleetManager{
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
	return nil
}

// Create starts a Gameye session and returns {CreateSessionIdKey: id}
// immediately. The API call runs in the background on a context detached from
// ctx (Nakama cancels the MatchmakerMatched hook's context as soon as the hook
// returns), bounded by GameyeConfig.CreateTimeout. The callback then receives
// CreateSuccess with the instance, CreateTimeout, or CreateError.
func (fm *GameyeFleetManager) Create(
	ctx context.Context,
	maxPlayers int,
	userIds []string,
	latencies []runtime.FleetUserLatencies,
	metadata map[string]any,
	callback runtime.FmCreateCallbackFn,
) (map[string]string, error) {
	var externalId string
	labels := make(map[string]string)
	for key, rawValue := range metadata {
		if key == MetadataKeyExternalId {
			value, ok := rawValue.(string)
			if !ok {
				return nil, fmt.Errorf("metadata %q must be a string, got %T", MetadataKeyExternalId, rawValue)
			}
			externalId = value
			continue
		}

		switch value := rawValue.(type) {
		case uint8, uint16, uint32, uint64, int8, int16, int32, int64, int, string, bool, float64, float32:
			labels[key] = fmt.Sprint(value)

		default:
			bytes, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("error marshalling metadata: %v: %v", err, value)
			}

			labels[key] = string(bytes)
		}
	}

	id := fm.callbackHandler.GenerateCallbackId()

	requestBody := gameye.SessionRun{
		ID:         id,
		Region:     fm.config.Region,
		Image:      fm.config.Image,
		Tag:        fm.config.Version,
		Labels:     labels,
		Ttl:        fm.config.Ttl,
		ExternalID: externalId,
	}

	if callback != nil {
		fm.callbackHandler.SetCallback(id, callback)
	}

	go fm.startSession(context.WithoutCancel(ctx), requestBody)

	return map[string]string{CreateSessionIdKey: id}, nil
}

func (fm *GameyeFleetManager) startSession(ctx context.Context, req gameye.SessionRun) {
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

	// The id we requested is authoritative; the response id is optional.
	instanceInfo := &runtime.InstanceInfo{
		Id:          id,
		CreateTime:  time.Now(),
		PlayerCount: 0,
		Status:      string(gameyeApi.Running),
		ConnectionInfo: &runtime.ConnectionInfo{
			IpAddress: started.Host,
			Port:      port,
		},
	}

	// Store before invoking the callback, so a Join from the callback finds it.
	storeCtx, cancel := context.WithTimeout(ctx, fm.config.CreateTimeout)
	if err := fm.writeToStorage(storeCtx, []*runtime.InstanceInfo{instanceInfo}); err != nil {
		fm.logger.Error("error writing gameye session %v to nakama storage: %v", id, err)
	}
	cancel()

	fm.callbackHandler.InvokeCallback(id, runtime.CreateSuccess, instanceInfo, nil, nil, nil)
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

	return fm.deleteFromStorage(ctx, []string{id})
}

func (fm *GameyeFleetManager) Get(
	ctx context.Context,
	id string,
) (instance *runtime.InstanceInfo, err error) {
	session, err := fm.apiClient.SessionDescribe(ctx, gameye.SessionDescribe{ID: id})
	if err != nil {
		return nil, fmt.Errorf("error describing session: %v: %v", id, err)
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
	}

	switch session.Status {
	case gameyeApi.Running, gameyeApi.Draining, gameyeApi.Shuttingdown:
		if err = fm.writeToStorage(ctx, []*runtime.InstanceInfo{instance}); err != nil {
			return nil, err
		}

	default:
		if err = fm.deleteFromStorage(ctx, []string{session.ID}); err != nil {
			return nil, err
		}
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
		return list, nextCursor, fmt.Errorf("error listing session: %v", err)
	}

	var instances []*runtime.InstanceInfo
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
		}

		instances = append(instances, instance)
	}

	if err = fm.writeToStorage(ctx, instances); err != nil {
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
	instanceInfo, err := fm.readFromStorage(ctx, id)
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
		return nil, fmt.Errorf("error joining session %v: %v", id, err)
	}

	var sessionInfo []*runtime.SessionInfo
	for _, playerId := range players {
		sessionInfo = append(sessionInfo, &runtime.SessionInfo{
			UserId:    playerId,
			SessionId: id,
		})
	}

	instanceInfo.PlayerCount = len(players)
	if err = fm.writeToStorage(ctx, []*runtime.InstanceInfo{instanceInfo}); err != nil {
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
	instanceInfo, err := fm.readFromStorage(ctx, id)
	if err != nil {
		return err
	}

	if instanceInfo == nil {
		_, err = fm.Get(ctx, id)
		return err
	}

	instanceInfo.PlayerCount = playerCount
	err = fm.writeToStorage(ctx, []*runtime.InstanceInfo{instanceInfo})
	if err != nil {
		return err
	}

	return nil
}

// hostPort selects the configured port; 0 when the session doesn't expose it.
func (fm *GameyeFleetManager) hostPort(id string, ports map[string]int) int {
	port, ok := gameye.HostPort(ports, fm.config.Port)
	if !ok {
		fm.logger.Warn("gameye session %v exposes no port matching %q (ports: %v)", id, fm.config.Port, ports)
	}
	return port
}

func (fm *GameyeFleetManager) readFromStorage(ctx context.Context, id string) (*runtime.InstanceInfo, error) {
	objects, err := fm.nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: StorageGameyeInstancesCollection,
		Key:        id,
	}})

	if err != nil {
		return nil, err
	}

	if len(objects) == 0 {
		return nil, nil
	}

	obj := objects[0]

	var instance *runtime.InstanceInfo
	if err = json.Unmarshal([]byte(obj.Value), &instance); err != nil {
		return nil, err
	}

	return instance, nil
}

func (fm *GameyeFleetManager) writeToStorage(ctx context.Context, instances []*runtime.InstanceInfo) error {
	storageWrites := make([]*runtime.StorageWrite, 0, len(instances))
	for _, i := range instances {
		v, err := json.Marshal(i)
		if err != nil {
			return err
		}

		storageWrites = append(storageWrites, &runtime.StorageWrite{
			Collection: StorageGameyeInstancesCollection,
			Key:        i.Id,
			Value:      string(v),
		})
	}

	if _, err := fm.nk.StorageWrite(ctx, storageWrites); err != nil {
		return err
	}

	return nil
}

func (fm *GameyeFleetManager) deleteFromStorage(ctx context.Context, ids []string) error {
	deletes := make([]*runtime.StorageDelete, 0, len(ids))
	for _, id := range ids {
		deletes = append(deletes, &runtime.StorageDelete{
			Collection: StorageGameyeInstancesCollection,
			Key:        id,
		})
	}

	if err := fm.nk.StorageDelete(ctx, deletes); err != nil {
		return err
	}

	return nil
}
