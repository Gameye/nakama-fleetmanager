package fleetmanager

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Gameye/nakama-fleetmanager/gameye"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// --- fakes -----------------------------------------------------------------

type fakeApi struct {
	mu      sync.Mutex
	runs    []gameye.SessionRun
	stops   []string
	runFn   func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error)
	listFn  func(ctx context.Context, req gameye.SessionList) ([]gameye.SessionListEntry, error)
	descFn  func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error)
	stopped chan string
}

func newFakeApi() *fakeApi {
	return &fakeApi{stopped: make(chan string, 10)}
}

func (f *fakeApi) SessionRun(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
	f.mu.Lock()
	f.runs = append(f.runs, req)
	f.mu.Unlock()
	if f.runFn != nil {
		return f.runFn(ctx, req)
	}
	return &gameye.SessionStarted{ID: req.ID, Host: "1.2.3.4", Ports: []gameye.Port{{Type: "tcp", Container: 7360, Host: 21000}}}, nil
}

func (f *fakeApi) SessionStop(ctx context.Context, req gameye.SessionStop) error {
	f.mu.Lock()
	f.stops = append(f.stops, req.ID)
	f.mu.Unlock()
	f.stopped <- req.ID
	return nil
}

func (f *fakeApi) SessionList(ctx context.Context, req gameye.SessionList) ([]gameye.SessionListEntry, error) {
	return f.listFn(ctx, req)
}

func (f *fakeApi) SessionDescribe(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
	return f.descFn(ctx, req)
}

func (f *fakeApi) SessionJoin(ctx context.Context, req gameye.SessionJoin) ([]string, error) {
	return req.PlayerIDs, nil
}

func (f *fakeApi) lastRun(t *testing.T) gameye.SessionRun {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runs) == 0 {
		t.Fatalf("SessionRun was not called")
	}
	return f.runs[len(f.runs)-1]
}

type callbackResult struct {
	id       string
	status   runtime.FmCreateStatus
	instance *runtime.InstanceInfo
	sessions []*runtime.SessionInfo
	err      error
}

type fakeCallbackHandler struct {
	mu        sync.Mutex
	next      int
	callbacks map[string]runtime.FmCreateCallbackFn
	results   chan callbackResult
}

func newFakeCallbackHandler() *fakeCallbackHandler {
	return &fakeCallbackHandler{callbacks: map[string]runtime.FmCreateCallbackFn{}, results: make(chan callbackResult, 10)}
}

func (h *fakeCallbackHandler) GenerateCallbackId() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	return "session-" + string(rune('0'+h.next))
}

func (h *fakeCallbackHandler) SetCallback(id string, fn runtime.FmCreateCallbackFn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.callbacks[id] = fn
}

func (h *fakeCallbackHandler) InvokeCallback(id string, status runtime.FmCreateStatus, instanceInfo *runtime.InstanceInfo, sessionInfo []*runtime.SessionInfo, metadata map[string]any, err error) {
	h.mu.Lock()
	fn := h.callbacks[id]
	delete(h.callbacks, id)
	h.mu.Unlock()
	if fn != nil {
		fn(status, instanceInfo, sessionInfo, metadata, err)
	}
	h.results <- callbackResult{id: id, status: status, instance: instanceInfo, sessions: sessionInfo, err: err}
}

func (h *fakeCallbackHandler) wait(t *testing.T) callbackResult {
	t.Helper()
	select {
	case r := <-h.results:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("callback was not invoked")
		return callbackResult{}
	}
}

// fakeNk implements only the storage calls the fleet manager uses; any other
// NakamaModule call panics on the nil embedded interface.
type fakeNk struct {
	runtime.NakamaModule
	mu       sync.Mutex
	objects  map[string]string
	ctxErrs  []error
	writeErr error
}

func newFakeNk() *fakeNk { return &fakeNk{objects: map[string]string{}} }

func (n *fakeNk) StorageWrite(ctx context.Context, writes []*runtime.StorageWrite) ([]*api.StorageObjectAck, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		n.ctxErrs = append(n.ctxErrs, err)
		return nil, err
	}
	for _, w := range writes {
		n.objects[w.Key] = w.Value
	}
	return nil, n.writeErr
}

func (n *fakeNk) StorageRead(ctx context.Context, reads []*runtime.StorageRead) ([]*api.StorageObject, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []*api.StorageObject
	for _, r := range reads {
		if v, ok := n.objects[r.Key]; ok {
			out = append(out, &api.StorageObject{Collection: r.Collection, Key: r.Key, Value: v})
		}
	}
	return out, nil
}

func (n *fakeNk) StorageDelete(ctx context.Context, deletes []*runtime.StorageDelete) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, d := range deletes {
		delete(n.objects, d.Key)
	}
	return nil
}

func (n *fakeNk) stored(t *testing.T, id string) *runtime.InstanceInfo {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.objects[id]
	if !ok {
		return nil
	}
	var i runtime.InstanceInfo
	if err := json.Unmarshal([]byte(v), &i); err != nil {
		t.Fatalf("stored value not json: %v", err)
	}
	return &i
}

type nopLogger struct{ runtime.Logger }

func (nopLogger) Debug(string, ...interface{}) {}
func (nopLogger) Info(string, ...interface{})  {}
func (nopLogger) Warn(string, ...interface{})  {}
func (nopLogger) Error(string, ...interface{}) {}

func testConfig() GameyeConfig {
	return GameyeConfig{
		BaseUrl:  "http://unused",
		ApiToken: "token",
		Region:   "europe",
		Image:    "my-game",
		Version:  "1.0.0",
	}
}

func newTestFleetManager(t *testing.T, cfg GameyeConfig, apiClient gameye.ApiClient) (*GameyeFleetManager, *fakeNk, *fakeCallbackHandler) {
	t.Helper()
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	nk := newFakeNk()
	handler := newFakeCallbackHandler()
	fm := &GameyeFleetManager{config: cfg, logger: nopLogger{}, apiClient: apiClient}
	if err := fm.Init(nk, handler); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return fm, nk, handler
}

// --- Create ----------------------------------------------------------------

func TestCreateReturnsSessionId(t *testing.T) {
	fapi := newFakeApi()
	fm, _, handler := newTestFleetManager(t, testConfig(), fapi)

	out, err := fm.Create(context.Background(), 2, nil, nil, nil, func(runtime.FmCreateStatus, *runtime.InstanceInfo, []*runtime.SessionInfo, map[string]any, error) {})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out[CreateSessionIdKey] == "" {
		t.Fatalf("Create returned %v, want a %q entry", out, CreateSessionIdKey)
	}

	res := handler.wait(t)
	if res.status != runtime.CreateSuccess {
		t.Fatalf("status = %v, err = %v", res.status, res.err)
	}
	if res.instance.Id != out[CreateSessionIdKey] {
		t.Fatalf("instance id %q != returned id %q", res.instance.Id, out[CreateSessionIdKey])
	}
	if got := fapi.lastRun(t).ID; got != out[CreateSessionIdKey] {
		t.Fatalf("requested id %q != returned id %q", got, out[CreateSessionIdKey])
	}
}

func TestCreateSurvivesCallerContextCancellation(t *testing.T) {
	release := make(chan struct{})
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		<-release
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &gameye.SessionStarted{ID: req.ID, Host: "1.2.3.4", Ports: []gameye.Port{{Type: "tcp", Container: 7360, Host: 21000}}}, nil
	}
	fm, nk, handler := newTestFleetManager(t, testConfig(), fapi)

	// Nakama 3.39+ cancels the MatchmakerMatched context when the hook returns.
	ctx, cancel := context.WithCancel(context.Background())
	out, err := fm.Create(ctx, 2, nil, nil, nil, nil)
	cancel()
	close(release)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	res := handler.wait(t)
	if res.status != runtime.CreateSuccess {
		t.Fatalf("status = %v, err = %v; want CreateSuccess", res.status, res.err)
	}
	if nk.stored(t, out[CreateSessionIdKey]) == nil {
		t.Fatalf("instance was not written to storage (ctx errors: %v)", nk.ctxErrs)
	}
}

func TestCreateTimesOut(t *testing.T) {
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cfg := testConfig()
	cfg.CreateTimeout = 50 * time.Millisecond
	fm, nk, handler := newTestFleetManager(t, cfg, fapi)

	start := time.Now()
	out, err := fm.Create(context.Background(), 2, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	res := handler.wait(t)
	if res.status != runtime.CreateTimeout {
		t.Fatalf("status = %v, err = %v; want CreateTimeout", res.status, res.err)
	}
	if res.instance != nil {
		t.Fatalf("instance must be nil on timeout")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("timed out after %v, before the configured timeout", elapsed)
	}
	// The session might still start on Gameye's side; it is stopped best-effort.
	select {
	case id := <-fapi.stopped:
		if id != out[CreateSessionIdKey] {
			t.Fatalf("stopped %q, want %q", id, out[CreateSessionIdKey])
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed-out session was not stopped")
	}
	if nk.stored(t, out[CreateSessionIdKey]) != nil {
		t.Fatalf("timed-out session must not be stored")
	}
}

func TestCreateUsesRequestedIdWhenResponseHasNone(t *testing.T) {
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		return &gameye.SessionStarted{Host: "1.2.3.4", Ports: []gameye.Port{{Type: "tcp", Container: 7360, Host: 21000}}}, nil
	}
	fm, nk, handler := newTestFleetManager(t, testConfig(), fapi)

	out, err := fm.Create(context.Background(), 2, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateSuccess {
		t.Fatalf("status = %v, err = %v", res.status, res.err)
	}
	if res.instance.Id != out[CreateSessionIdKey] {
		t.Fatalf("instance id = %q, want requested %q", res.instance.Id, out[CreateSessionIdKey])
	}
	if nk.stored(t, out[CreateSessionIdKey]) == nil {
		t.Fatalf("instance not stored under the requested id")
	}
}

func TestCreateSendsTtlExternalIdAndVersion(t *testing.T) {
	fapi := newFakeApi()
	fm, _, handler := newTestFleetManager(t, testConfig(), fapi)

	_, err := fm.Create(context.Background(), 2, nil, nil, map[string]any{
		MetadataKeyExternalId: "match-42",
		"mode":                "duel",
	}, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	handler.wait(t)

	run := fapi.lastRun(t)
	if run.Ttl != DefaultTtl {
		t.Errorf("Ttl = %q, want default %q", run.Ttl, DefaultTtl)
	}
	if run.ExternalID != "match-42" {
		t.Errorf("ExternalID = %q", run.ExternalID)
	}
	if run.Tag != "1.0.0" || run.Region != "europe" || run.Image != "my-game" {
		t.Errorf("unexpected run %+v", run)
	}
	if run.Labels["mode"] != "duel" {
		t.Errorf("labels = %v", run.Labels)
	}
	if _, ok := run.Labels[MetadataKeyExternalId]; ok {
		t.Errorf("reserved key leaked into labels: %v", run.Labels)
	}
}

func TestCreateConfiguredTtl(t *testing.T) {
	fapi := newFakeApi()
	cfg := testConfig()
	cfg.Ttl = "2h"
	fm, _, handler := newTestFleetManager(t, cfg, fapi)
	if _, err := fm.Create(context.Background(), 2, nil, nil, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	handler.wait(t)
	if got := fapi.lastRun(t).Ttl; got != "2h" {
		t.Fatalf("Ttl = %q, want 2h", got)
	}
}

func TestCreateRejectsNonStringExternalId(t *testing.T) {
	fapi := newFakeApi()
	fm, _, _ := newTestFleetManager(t, testConfig(), fapi)
	if _, err := fm.Create(context.Background(), 2, nil, nil, map[string]any{MetadataKeyExternalId: 7}, nil); err == nil {
		t.Fatalf("expected an error for a non-string external id")
	}
	if len(fapi.runs) != 0 {
		t.Fatalf("no API call expected")
	}
}

func TestCreateSelectsConfiguredPort(t *testing.T) {
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		return &gameye.SessionStarted{ID: req.ID, Host: "1.2.3.4", Ports: []gameye.Port{
			{Type: "tcp", Container: 9000, Host: 20001},
			{Type: "tcp", Container: 7360, Host: 20002},
		}}, nil
	}
	cfg := testConfig()
	cfg.Port = "7360/tcp"
	fm, _, handler := newTestFleetManager(t, cfg, fapi)

	if _, err := fm.Create(context.Background(), 2, nil, nil, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateSuccess {
		t.Fatalf("status = %v, err = %v", res.status, res.err)
	}
	if res.instance.ConnectionInfo.Port != 20002 || res.instance.ConnectionInfo.IpAddress != "1.2.3.4" {
		t.Fatalf("connection info = %+v, want 1.2.3.4:20002", res.instance.ConnectionInfo)
	}
}

func TestCreateMissingConfiguredPortStopsSession(t *testing.T) {
	fapi := newFakeApi()
	cfg := testConfig()
	cfg.Port = "7777/udp"
	fm, nk, handler := newTestFleetManager(t, cfg, fapi)

	out, err := fm.Create(context.Background(), 2, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateError || res.err == nil {
		t.Fatalf("status = %v, err = %v; want CreateError", res.status, res.err)
	}
	select {
	case id := <-fapi.stopped:
		if id != out[CreateSessionIdKey] {
			t.Fatalf("stopped %q", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("session without the configured port was not stopped")
	}
	if nk.stored(t, out[CreateSessionIdKey]) != nil {
		t.Fatalf("session must not be stored")
	}
}

func TestCreateApiErrorIsCreateErrorWithSentinel(t *testing.T) {
	fapi := newFakeApi()
	fapi.runFn = func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error) {
		return nil, &gameye.ApiError{StatusCode: 420}
	}
	fm, _, handler := newTestFleetManager(t, testConfig(), fapi)

	if _, err := fm.Create(context.Background(), 2, nil, nil, nil, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	res := handler.wait(t)
	if res.status != runtime.CreateError || !errors.Is(res.err, gameye.ErrNoCapacity) {
		t.Fatalf("status = %v, err = %v; want CreateError wrapping ErrNoCapacity", res.status, res.err)
	}
}

// --- Get / List port selection --------------------------------------------

func TestGetAndListSelectConfiguredPort(t *testing.T) {
	ports := map[string]int{"9000/tcp": 20001, "7360/tcp": 20002, "7360/udp": 20003}
	fapi := newFakeApi()
	fapi.descFn = func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
		return &gameye.Session{ID: req.ID, Status: "running", IPV4Address: "1.2.3.4", Ports: ports}, nil
	}
	fapi.listFn = func(ctx context.Context, req gameye.SessionList) ([]gameye.SessionListEntry, error) {
		return []gameye.SessionListEntry{{ID: "a", Status: "running", IPV4Address: "1.2.3.4", Ports: ports}}, nil
	}
	cfg := testConfig()
	cfg.Port = "7360/udp"
	fm, _, _ := newTestFleetManager(t, cfg, fapi)

	for i := 0; i < 20; i++ {
		inst, err := fm.Get(context.Background(), "a")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if inst.ConnectionInfo.Port != 20003 {
			t.Fatalf("Get port = %d, want 20003", inst.ConnectionInfo.Port)
		}
		list, _, err := fm.List(context.Background(), "", 0, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if list[0].ConnectionInfo.Port != 20003 {
			t.Fatalf("List port = %d, want 20003", list[0].ConnectionInfo.Port)
		}
	}
}

// --- config ----------------------------------------------------------------

func TestConfigDefaultsAndValidation(t *testing.T) {
	cfg := GameyeConfig{ApiToken: "t", Region: "r", Image: "i", Version: "v"}.withDefaults()
	if cfg.BaseUrl != gameye.DefaultBaseUrl {
		t.Errorf("BaseUrl default = %q", cfg.BaseUrl)
	}
	if cfg.Ttl != DefaultTtl {
		t.Errorf("Ttl default = %q", cfg.Ttl)
	}
	if cfg.CreateTimeout != DefaultCreateTimeout {
		t.Errorf("CreateTimeout default = %v", cfg.CreateTimeout)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	bad := cfg
	bad.Port = "7360"
	if err := bad.Validate(); !errors.Is(err, ErrInvalidPort) {
		t.Errorf("port without protocol: err = %v", err)
	}
	bad = cfg
	bad.Ttl = "30s"
	if err := bad.Validate(); !errors.Is(err, ErrInvalidTtl) {
		t.Errorf("ttl in seconds: err = %v", err)
	}
	for _, ok := range []string{"30m", "1h", "2h30m"} {
		good := cfg
		good.Ttl = ok
		if err := good.Validate(); err != nil {
			t.Errorf("ttl %q rejected: %v", ok, err)
		}
	}
}
