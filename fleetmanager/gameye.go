package fleetmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	"gitlab.com/gameye/nakama/gameye"
	gameyeApi "gitlab.com/gameye/nakama/pkg/api/generated/openapi/client"
)

const (
	StorageGameyeInstancesCollection = "_gameye_instances"
)

var (
	ErrSecurityProvider = errors.New("error creating securityprovider")
	ErrCreateClient     = errors.New("error creating gameye sdk client")
	ErrNoBaseUrl        = errors.New("no base url provided")
	ErrNoApiToken       = errors.New("no api token provided")
	ErrNoRegion         = errors.New("no region provided")
	ErrNoImage          = errors.New("no image provided")
	ErrNoVersion        = errors.New("no image version provided")
)

type GameyeConfig struct {
	BaseUrl  string
	ApiToken string
	Region   string
	Image    string
	Version  string
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

func (fm *GameyeFleetManager) Create(
	ctx context.Context,
	maxPlayers int,
	userIds []string,
	latencies []runtime.FleetUserLatencies,
	metadata map[string]any,
	callback runtime.FmCreateCallbackFn,
) (err error) {
	labels := make(map[string]string)
	for key, rawValue := range metadata {
		switch value := rawValue.(type) {
		case uint8, uint16, uint32, uint64, int8, int16, int32, int64, int, string, bool, float64, float32:
			labels[key] = fmt.Sprint(value)

		default:
			bytes, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("error marshalling metadata: %v: %v", err, value)
			}

			labels[key] = string(bytes)
		}
	}

	var (
		id          = fm.callbackHandler.GenerateCallbackId()
		programArgs = make([]string, 0)
		envVars     = make(map[string]string)
		restart     = false
	)

	requestBody := gameye.SessionRun{
		ID:          id,
		Region:      fm.config.Region,
		Image:       fm.config.Image,
		EnvVars:     envVars,
		ProgramArgs: programArgs,
		Tag:         fm.config.Version,
		Labels:      labels,
		Restart:     restart,
	}

	if callback != nil {
		fm.callbackHandler.SetCallback(id, callback)
	}

	go func(
		ctx context.Context,
		callbackId string,
		callbackHandler runtime.FmCallbackHandler,
	) {
		ok, err := fm.apiClient.SessionRun(ctx, requestBody)
		if err != nil {
			go callbackHandler.InvokeCallback(
				callbackId,
				runtime.CreateError,
				nil, nil,
				make(map[string]any),
				err,
			)

			return
		}

		connectionInfo := &runtime.ConnectionInfo{
			IpAddress: ok.Host,
			Port:      ok.Ports[0].Host,
		}

		instanceInfo := &runtime.InstanceInfo{
			Id:             ok.ID,
			CreateTime:     time.Now(),
			PlayerCount:    0,
			Status:         string(gameyeApi.Running),
			ConnectionInfo: connectionInfo,
		}

		sessionInfo := []*runtime.SessionInfo{}

		go callbackHandler.InvokeCallback(
			id,
			runtime.CreateSuccess,
			instanceInfo,
			sessionInfo,
			make(map[string]any),
			nil,
		)

		if err = fm.writeToStorage(ctx, []*runtime.InstanceInfo{instanceInfo}); err != nil {
			fm.logger.Error(err.Error(), "error writing to nakama storage after starting session")
		}
	}(ctx, id, fm.callbackHandler)

	return nil
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
		Port:      session.Port,
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
			Port:      session.Port,
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
