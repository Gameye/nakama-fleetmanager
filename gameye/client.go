package gameye

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	gameyeApi "github.com/Gameye/nakama-fleetmanager/pkg/api/generated/openapi/client"
	"github.com/oapi-codegen/oapi-codegen/v2/pkg/securityprovider"
)

// DefaultBaseUrl is Gameye's self-serve Session API, where trial accounts
// live. Production contracts use https://api.production-gameye.gameye.net.
const DefaultBaseUrl = "https://api.sandbox-gameye.gameye.net"

// StatusNoCapacity is the non-standard status Gameye returns when a region has
// no capacity left for the requested session.
const StatusNoCapacity = 420

// Sentinel errors returned (wrapped in *ApiError) by the ApiClient. Match them
// with errors.Is.
var (
	// ErrUnauthorized: the API token is missing, invalid or expired (401).
	ErrUnauthorized = errors.New("gameye: unauthorized")
	// ErrQuotaExceeded: the organization's session quota is exhausted (402).
	ErrQuotaExceeded = errors.New("gameye: quota exceeded")
	// ErrForbidden: the API token lacks the scope this call needs (403).
	ErrForbidden = errors.New("gameye: token lacks the required scope")
	// ErrNotFound: the session, region, application or tag does not exist (404).
	ErrNotFound = errors.New("gameye: not found")
	// ErrNoCapacity: no capacity is available in the requested region (420).
	ErrNoCapacity = errors.New("gameye: no capacity available in region")
	// ErrInternalServer: Gameye returned a 5xx. These errors are retryable.
	ErrInternalServer = errors.New("gameye: internal server error")

	// Deprecated: use ErrNoCapacity.
	ErrRanOutOfCompute = ErrNoCapacity
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
	// Ttl is the maximum session lifetime, for example "30m" or "2h". Empty
	// means no TTL.
	Ttl string
	// ExternalID is an optional caller-provided identifier stored with the
	// session.
	ExternalID string
}

// ApiError is returned for every non-success response from the Gameye API.
// It wraps the sentinel matching its status code, so errors.Is works.
type ApiError struct {
	StatusCode int
	Details    string
	Message    string
}

func (e ApiError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.Details != "" {
		return fmt.Sprintf("gameye api error %d: %s: %s", e.StatusCode, msg, e.Details)
	}
	return fmt.Sprintf("gameye api error %d: %s", e.StatusCode, msg)
}

// Unwrap returns the sentinel error for the status code, or nil.
func (e ApiError) Unwrap() error {
	switch {
	case e.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case e.StatusCode == http.StatusPaymentRequired:
		return ErrQuotaExceeded
	case e.StatusCode == http.StatusForbidden:
		return ErrForbidden
	case e.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case e.StatusCode == StatusNoCapacity:
		return ErrNoCapacity
	case e.StatusCode >= 500:
		return ErrInternalServer
	default:
		return nil
	}
}

// Retryable reports whether the same request may succeed if sent again.
func (e ApiError) Retryable() bool {
	return e.StatusCode >= 500
}

// IsRetryable reports whether err is a Gameye API error worth retrying (5xx).
func IsRetryable(err error) bool {
	var apiErr *ApiError
	return errors.As(err, &apiErr) && apiErr.Retryable()
}

type Port struct {
	Type      string
	Container int
	Host      int
}

// Key returns the port in Gameye's "<container>/<protocol>" form, e.g. "7360/tcp".
func (p Port) Key() string {
	return fmt.Sprintf("%d/%s", p.Container, p.Type)
}

type SessionStarted struct {
	ID    string
	Host  string
	Ports []Port
}

// PortMap returns the ports keyed by "<container>/<protocol>" with the host
// port as value, the same shape list and describe return.
func (s SessionStarted) PortMap() map[string]int {
	ports := make(map[string]int, len(s.Ports))
	for _, p := range s.Ports {
		ports[p.Key()] = p.Host
	}
	return ports
}

// HostPort picks the host port for a session. With a key such as "7360/tcp"
// it returns that port's mapping. With an empty key it picks the lowest
// container port (tcp before udp), so the choice never depends on map order.
func HostPort(ports map[string]int, key string) (int, bool) {
	if key != "" {
		port, ok := ports[key]
		return port, ok
	}

	if len(ports) == 0 {
		return 0, false
	}

	keys := make([]string, 0, len(ports))
	for k := range ports {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ci, pi := splitPortKey(keys[i])
		cj, pj := splitPortKey(keys[j])
		if ci != cj {
			return ci < cj
		}
		if pi != pj {
			return pi < pj
		}
		return keys[i] < keys[j]
	})

	return ports[keys[0]], true
}

func splitPortKey(key string) (int, string) {
	num, proto, _ := strings.Cut(key, "/")
	n, err := strconv.Atoi(num)
	if err != nil {
		n = math.MaxInt
	}
	return n, proto
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
	// Ports maps "<container>/<protocol>" to the host port. Use HostPort to
	// select one.
	Ports map[string]int
	// Labels are the session's labels as Gameye reports them. Gameye may
	// include the container env under the "env" key.
	Labels map[string]string
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
	// Ports maps "<container>/<protocol>" to the host port. Use HostPort to
	// select one.
	Ports map[string]int
	// Labels are the session's labels as Gameye reports them. Gameye may
	// include the container env under the "env" key.
	Labels map[string]string
}

type SessionJoin struct {
	ID        string
	PlayerIDs []string
}

type ApiClient interface {
	SessionRun(ctx context.Context, req SessionRun) (*SessionStarted, error)

	// SessionStop stops a session. A session that is already gone (404) or
	// already stopping (409) counts as stopped and returns nil.
	SessionStop(ctx context.Context, req SessionStop) error

	SessionList(ctx context.Context, req SessionList) ([]SessionListEntry, error)

	SessionDescribe(ctx context.Context, req SessionDescribe) (*Session, error)

	SessionJoin(ctx context.Context, req SessionJoin) ([]string, error)
}

type defaultApiClient struct {
	apiClient *gameyeApi.Client
}

// NewApiClient returns a client for the Gameye Session API. An empty baseUrl
// uses DefaultBaseUrl.
func NewApiClient(baseUrl, apiToken string) (ApiClient, error) {
	if baseUrl == "" {
		baseUrl = DefaultBaseUrl
	}

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
		Location: req.Region,
		Image:    req.Image,
		Labels:   req.Labels,
		Restart:  &req.Restart,
		Id:       optionalString(req.ID),
		Version:  optionalString(req.Tag),
		Ttl:      optionalString(req.Ttl),
		// external_id is optional; omit it rather than send "".
		ExternalId: optionalString(req.ExternalID),
	}
	if len(req.EnvVars) > 0 {
		env := req.EnvVars
		requestBody.Env = &env
	}
	if len(req.ProgramArgs) > 0 {
		args := req.ProgramArgs
		requestBody.Args = &args
	}

	response, err := d.apiClient.SessionRun(ctx, requestBody)
	if err != nil {
		return nil, fmt.Errorf("error calling gameye session-run: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		return nil, readApiError(response)
	}

	ok := &gameyeApi.SessionRunOkBody{}
	if err := decodeBody(response.Body, ok); err != nil {
		return nil, fmt.Errorf("error decoding session-run response: %w", err)
	}

	ports := make([]Port, 0, len(ok.Ports))
	for _, port := range ok.Ports {
		ports = append(ports, Port{
			Type:      string(port.Type),
			Host:      port.Host,
			Container: port.Container,
		})
	}

	id := req.ID
	if ok.Id != nil && *ok.Id != "" {
		id = *ok.Id
	}

	return &SessionStarted{
		ID:    id,
		Host:  ok.Host,
		Ports: ports,
	}, nil
}

func (d *defaultApiClient) SessionStop(ctx context.Context, req SessionStop) error {
	response, err := d.apiClient.SessionStop(ctx, req.ID)
	if err != nil {
		return fmt.Errorf("error stopping session %v: %w", req.ID, err)
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound, http.StatusConflict:
		return nil
	default:
		return readApiError(response)
	}
}

// sessionListBody mirrors gameyeApi.SessionListOkBody but decodes "created"
// (milliseconds since epoch) as float64; the generated float32 loses minutes.
type sessionListBody struct {
	Sessions []struct {
		gameyeApi.SessionListEntry
		Created float64 `json:"created"`
	} `json:"sessions"`
}

func (d *defaultApiClient) SessionList(ctx context.Context, req SessionList) ([]SessionListEntry, error) {
	params := &gameyeApi.SessionListParams{
		Location: optionalString(req.Region),
		Image:    optionalString(req.Image),
		Tag:      optionalString(req.Tag),
	}

	response, err := d.apiClient.SessionList(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("error listing sessions: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, readApiError(response)
	}

	ok := &sessionListBody{}
	if err := decodeBody(response.Body, ok); err != nil {
		return nil, fmt.Errorf("error decoding session-list response: %w", err)
	}

	sessions := make([]SessionListEntry, 0, len(ok.Sessions))
	for _, session := range ok.Sessions {
		var playerCount int
		if session.PlayerCount != nil {
			playerCount = *session.PlayerCount
		}
		var labels map[string]string
		if session.Labels != nil {
			labels = *session.Labels
		}

		sessions = append(sessions, SessionListEntry{
			ID:          session.Id,
			Created:     time.UnixMilli(int64(session.Created)),
			PlayerCount: playerCount,
			Status:      string(session.Status),
			IPV4Address: session.Host,
			Ports:       portMap(session.Port),
			Labels:      labels,
		})
	}

	return sessions, nil
}

type describedSessionBody struct {
	gameyeApi.DescribedSession
	Created float64 `json:"created"`
}

func (d *defaultApiClient) SessionDescribe(ctx context.Context, req SessionDescribe) (*Session, error) {
	response, err := d.apiClient.DescribeSession(ctx, req.ID)
	if err != nil {
		return nil, fmt.Errorf("error describing session %v: %w", req.ID, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, readApiError(response)
	}

	ok := &describedSessionBody{}
	if err := decodeBody(response.Body, ok); err != nil {
		return nil, fmt.Errorf("error decoding describe-session response: %w", err)
	}

	return &Session{
		ID:          ok.Id,
		Created:     time.UnixMilli(int64(ok.Created)),
		PlayerCount: ok.Players.JoinedCount,
		Status:      ok.Status,
		IPV4Address: ok.Host,
		Ports:       portMap(ok.Port),
		Labels:      ok.Labels,
	}, nil
}

func (d *defaultApiClient) SessionJoin(ctx context.Context, req SessionJoin) ([]string, error) {
	response, err := d.apiClient.JoinSession(ctx, gameyeApi.JoinSession{
		Players: req.PlayerIDs,
		Session: req.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("error joining session %v: %w", req.ID, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, readApiError(response)
	}

	ok := &gameyeApi.JoinSessionOkBody{}
	if err := decodeBody(response.Body, ok); err != nil {
		return nil, fmt.Errorf("error decoding join-session response: %w", err)
	}

	return ok.Players, nil
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func portMap(in map[string]float32) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = int(v)
	}
	return out
}

func decodeBody(body io.Reader, v any) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: body: %s", err, truncate(b))
	}
	return nil
}

// readApiError builds an *ApiError from a non-success response. A body that is
// not a Gameye error document (a proxy's HTML page, say) still yields an
// *ApiError carrying the status code.
func readApiError(response *http.Response) error {
	apiErr := &ApiError{StatusCode: response.StatusCode}

	b, err := io.ReadAll(response.Body)
	if err != nil {
		apiErr.Details = fmt.Sprintf("error reading response body: %v", err)
		return apiErr
	}

	resp := &gameyeApi.ErrorResponse{}
	if err := json.Unmarshal(b, resp); err != nil {
		apiErr.Details = truncate(b)
		return apiErr
	}

	apiErr.Message = resp.Message
	apiErr.Details = resp.Details
	return apiErr
}

func truncate(b []byte) string {
	const max = 512
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
