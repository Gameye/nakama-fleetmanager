package gameye

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/oapi-codegen/oapi-codegen/v2/pkg/securityprovider"
	gameyeApi "gitlab.com/gameye/nakama/pkg/api/generated/openapi/client"
)

var (
	ErrRanOutOfCompute = errors.New("no compute available in specified region")
	ErrInternalServer  = errors.New("internal server error")
)

type SessionRun struct {
	ID          string
	Region      string
	Image       string
	Tag         string
	EnvVars     map[string]string
	ProgramArgs []string
	Labels      map[string]string
	Restart     bool
}

type ApiError struct {
	StatusCode int
	Details    string
	Message    string
}

func (e ApiError) Error() string {
	return e.Message
}

type Port struct {
	Type      string
	Container int
	Host      int
}

type SessionStarted struct {
	ID    string
	Host  string
	Ports []Port
}

type SessionStop struct {
	ID string
}

type SessionList struct {
	Region string
	Image  string
	Tag    string
}

type SessionListEntry struct {
	ID          string
	Created     time.Time
	PlayerCount int
	Status      string
	IPV4Address string
	Port        int
}

type SessionDescribe struct {
	ID string
}

type Session struct {
	ID          string
	Created     time.Time
	PlayerCount int
	Status      gameyeApi.SessionStatus
	IPV4Address string
	Port        int
}

type SessionJoin struct {
	ID        string
	PlayerIDs []string
}

type ApiClient interface {
	SessionRun(ctx context.Context, req SessionRun) (*SessionStarted, error)

	SessionStop(ctx context.Context, req SessionStop) error

	SessionList(ctx context.Context, req SessionList) ([]SessionListEntry, error)

	SessionDescribe(ctx context.Context, req SessionDescribe) (*Session, error)

	SessionJoin(ctx context.Context, req SessionJoin) ([]string, error)
}

type defaultApiClient struct {
	apiClient *gameyeApi.Client
}

func NewApiClient(baseUrl, apiToken string) (ApiClient, error) {
	auth, err := securityprovider.NewSecurityProviderBearerToken(apiToken)
	if err != nil {
		return nil, err
	}

	client, err := gameyeApi.NewClient(baseUrl, gameyeApi.WithRequestEditorFn(auth.Intercept))
	if err != nil {
		return nil, err
	}

	return &defaultApiClient{apiClient: client}, nil
}

func (d *defaultApiClient) SessionRun(ctx context.Context, req SessionRun) (*SessionStarted, error) {
	requestBody := gameyeApi.SessionRunJSONRequestBody{
		Id:       &req.ID,
		Location: req.Region,
		Image:    req.Image,
		Env:      &req.EnvVars,
		Args:     &req.ProgramArgs,
		Version:  &req.Tag,
		Labels:   req.Labels,
		Restart:  &req.Restart,
	}

	response, err := d.apiClient.SessionRun(ctx, requestBody)
	if err != nil {
		return nil, fmt.Errorf("error calling gameye session-run: %v", err)
	}

	switch {
	case response.StatusCode == http.StatusCreated:
		respBytes, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, fmt.Errorf("io error reading session-run response: %v", err)
		}

		response := &gameyeApi.SessionRunOk{}
		if err = json.Unmarshal(respBytes, response); err != nil {
			return nil, fmt.Errorf("error unmarshalling session-run response: %v json: %v", err, string(respBytes))
		}

		var ports []Port
		for _, port := range response.Ports {
			ports = append(ports, Port{
				Type:      string(port.Type),
				Host:      port.Host,
				Container: port.Container,
			})
		}

		result := &SessionStarted{
			ID:    *response.Id,
			Host:  response.Host,
			Ports: ports,
		}

		return result, nil

	case response.StatusCode >= 400 && response.StatusCode <= 500:
		resp, err := d.readErrorResponse(response.Body)
		if err != nil {
			return nil, err
		}

		return nil, &ApiError{
			StatusCode: resp.StatusCode,
			Message:    resp.Message,
			Details:    resp.Details,
		}

	default:
		return nil, fmt.Errorf("error creating session with status code: %v", response.StatusCode)
	}
}

func (d *defaultApiClient) SessionStop(ctx context.Context, req SessionStop) error {
	response, err := d.apiClient.SessionStop(ctx, req.ID)
	if err != nil {
		return fmt.Errorf("error stopping session %v: %v", req.ID, err)
	}

	switch response.StatusCode {
	case http.StatusNoContent:
		return nil

	default:
		resp, err := d.readErrorResponse(response.Body)
		if err != nil {
			return err
		}

		return &ApiError{
			StatusCode: resp.StatusCode,
			Message:    resp.Message,
			Details:    resp.Details,
		}
	}
}

func (d *defaultApiClient) SessionList(ctx context.Context, req SessionList) ([]SessionListEntry, error) {
	var (
		location *string
		image    *string
		tag      *string
	)

	if len(req.Region) > 0 {
		location = &req.Region
	}

	if len(req.Image) > 0 {
		image = &req.Image
	}

	if len(req.Tag) > 0 {
		tag = &req.Tag
	}

	params := &gameyeApi.SessionListParams{
		Location: location,
		Image:    image,
		Tag:      tag,
	}

	response, err := d.apiClient.SessionList(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("error listing session: %v", err)
	}

	switch response.StatusCode {
	case http.StatusOK:
		respBytes, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, fmt.Errorf("io error reading session-list response: %v", err)
		}

		ok := &gameyeApi.SessionListOk{}
		if err = json.Unmarshal(respBytes, ok); err != nil {
			return nil, fmt.Errorf("error unmarshalling session-list response: %v json: %v", err, string(respBytes))
		}

		var sessions []SessionListEntry
		for _, session := range ok.Sessions {
			var port int
			for _, value := range session.Port {
				port = int(value)
				break
			}

			entry := SessionListEntry{
				ID:          session.Id,
				Created:     time.Unix(int64(session.Created/1000), 0),
				PlayerCount: *session.PlayerCount,
				Status:      string(session.Status),
				IPV4Address: session.Host,
				Port:        port,
			}

			sessions = append(sessions, entry)
		}

		return sessions, nil

	default:
		resp, err := d.readErrorResponse(response.Body)
		if err != nil {
			return nil, err
		}

		return nil, &ApiError{
			StatusCode: resp.StatusCode,
			Message:    resp.Message,
			Details:    resp.Details,
		}
	}
}

func (d *defaultApiClient) SessionDescribe(ctx context.Context, req SessionDescribe) (*Session, error) {
	response, err := d.apiClient.DescribeSession(ctx, req.ID)
	if err != nil {
		return nil, fmt.Errorf("error describing session: %v: %v", req.ID, err)
	}

	switch response.StatusCode {
	case http.StatusOK:
		respBytes, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, fmt.Errorf("io error reading describe-session response: %v", err)
		}

		ok := &gameyeApi.DescribedSession{}
		if err = json.Unmarshal(respBytes, ok); err != nil {
			return nil, fmt.Errorf("error unmarshalling describe-session response: %v json: %v", err, string(respBytes))
		}

		var port int
		for _, value := range ok.Port {
			port = int(value)
			break
		}

		session := &Session{
			ID:          ok.Id,
			Created:     time.Unix(int64(ok.Created/1000), 0),
			PlayerCount: ok.Players.JoinedCount,
			Status:      ok.Status,
			IPV4Address: ok.Host,
			Port:        port,
		}

		return session, nil

	default:
		resp, err := d.readErrorResponse(response.Body)
		if err != nil {
			return nil, err
		}

		return nil, &ApiError{
			StatusCode: resp.StatusCode,
			Message:    resp.Message,
			Details:    resp.Details,
		}
	}
}

func (d *defaultApiClient) SessionJoin(ctx context.Context, req SessionJoin) ([]string, error) {
	response, err := d.apiClient.JoinSession(ctx, gameyeApi.JoinSession{
		Players: req.PlayerIDs,
		Session: req.ID,
	})

	if err != nil {
		return nil, fmt.Errorf("error joining session %v: %v", req.ID, err)
	}

	switch response.StatusCode {
	case http.StatusOK:
		respBytes, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, fmt.Errorf("io error reading join-session response: %v", err)
		}

		ok := &gameyeApi.JoinSessionOk{}
		if err = json.Unmarshal(respBytes, ok); err != nil {
			return nil, fmt.Errorf("error unmarshalling join-session response: %v json: %v", err, string(respBytes))
		}

		return ok.Players, nil

	default:
		resp, err := d.readErrorResponse(response.Body)
		if err != nil {
			return nil, err
		}

		return nil, &ApiError{
			StatusCode: resp.StatusCode,
			Message:    resp.Message,
			Details:    resp.Details,
		}
	}
}

func (d *defaultApiClient) readErrorResponse(body io.ReadCloser) (*gameyeApi.ErrorResponse, error) {
	respBytes, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("io error reading error response: %v", err)
	}

	resp := &gameyeApi.ErrorResponse{}
	if err = json.Unmarshal(respBytes, resp); err != nil {
		return nil, fmt.Errorf("error unmarshalling error response: %v json: %v", err, string(respBytes))
	}

	return resp, nil
}
