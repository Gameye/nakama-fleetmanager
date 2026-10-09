package fleetmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Gameye/nakama-fleetmanager/gameye"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Shared fakes for the fleet manager tests: an ApiClient, an InstanceStorage,
// an FmCallbackHandler and the few NakamaModule calls the package makes.

// --- ApiClient -------------------------------------------------------------

type fakeApi struct {
	mu        sync.Mutex
	runs      []gameye.SessionRun
	stops     []string
	lists     []gameye.SessionList
	describes []string
	joins     []gameye.SessionJoin
	runFn     func(ctx context.Context, req gameye.SessionRun) (*gameye.SessionStarted, error)
	stopErr   error
	listFn    func(ctx context.Context, req gameye.SessionList) ([]gameye.SessionListEntry, error)
	descFn    func(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error)
	joinFn    func(ctx context.Context, req gameye.SessionJoin) ([]string, error)
	stopped   chan string
}

var _ gameye.ApiClient = (*fakeApi)(nil)

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
	err := f.stopErr
	f.mu.Unlock()
	f.stopped <- req.ID
	return err
}

func (f *fakeApi) SessionList(ctx context.Context, req gameye.SessionList) ([]gameye.SessionListEntry, error) {
	f.mu.Lock()
	f.lists = append(f.lists, req)
	f.mu.Unlock()
	if f.listFn == nil {
		return nil, nil
	}
	return f.listFn(ctx, req)
}

func (f *fakeApi) SessionDescribe(ctx context.Context, req gameye.SessionDescribe) (*gameye.Session, error) {
	f.mu.Lock()
	f.describes = append(f.describes, req.ID)
	f.mu.Unlock()
	return f.descFn(ctx, req)
}

func (f *fakeApi) SessionJoin(ctx context.Context, req gameye.SessionJoin) ([]string, error) {
	f.mu.Lock()
	f.joins = append(f.joins, req)
	f.mu.Unlock()
	if f.joinFn != nil {
		return f.joinFn(ctx, req)
	}
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

func (f *fakeApi) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *fakeApi) joinCalls() []gameye.SessionJoin {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gameye.SessionJoin(nil), f.joins...)
}

func (f *fakeApi) stopCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stops...)
}

// --- FmCallbackHandler -----------------------------------------------------

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

var _ runtime.FmCallbackHandler = (*fakeCallbackHandler)(nil)

func newFakeCallbackHandler() *fakeCallbackHandler {
	return &fakeCallbackHandler{callbacks: map[string]runtime.FmCreateCallbackFn{}, results: make(chan callbackResult, 10)}
}

func (h *fakeCallbackHandler) GenerateCallbackId() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	return fmt.Sprintf("session-%d", h.next)
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

// --- InstanceStorage -------------------------------------------------------

// fakeStorage keeps instances as JSON, as Nakama storage does, so tests see
// exactly what a storage write would persist.
type fakeStorage struct {
	mu       sync.Mutex
	objects  map[string]string
	ctxErrs  []error
	writeErr error
	readErr  error
	listErr  error
	deleted  []string
}

var _ InstanceStorage = (*fakeStorage)(nil)

func newFakeStorage() *fakeStorage { return &fakeStorage{objects: map[string]string{}} }

func (s *fakeStorage) Read(ctx context.Context, id string) (*runtime.InstanceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	v, ok := s.objects[id]
	if !ok {
		return nil, nil
	}
	var i runtime.InstanceInfo
	if err := json.Unmarshal([]byte(v), &i); err != nil {
		return nil, err
	}
	return &i, nil
}

func (s *fakeStorage) Write(ctx context.Context, instances []*runtime.InstanceInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		s.ctxErrs = append(s.ctxErrs, err)
		return err
	}
	if s.writeErr != nil {
		return s.writeErr
	}
	for _, i := range instances {
		v, err := json.Marshal(i)
		if err != nil {
			return err
		}
		s.objects[i.Id] = string(v)
	}
	return nil
}

func (s *fakeStorage) Delete(ctx context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.objects, id)
		s.deleted = append(s.deleted, id)
	}
	return nil
}

func (s *fakeStorage) ListIds(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	ids := make([]string, 0, len(s.objects))
	for id := range s.objects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// put stores an instance directly, bypassing the fleet manager.
func (s *fakeStorage) put(t *testing.T, i *runtime.InstanceInfo) {
	t.Helper()
	v, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.objects[i.Id] = string(v)
	s.mu.Unlock()
}

// raw returns the stored JSON for id ("" when absent).
func (s *fakeStorage) raw(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[id]
}

func (s *fakeStorage) stored(t *testing.T, id string) *runtime.InstanceInfo {
	t.Helper()
	i, err := s.Read(context.Background(), id)
	if err != nil {
		t.Fatalf("stored value not json: %v", err)
	}
	return i
}

// --- NakamaModule ----------------------------------------------------------

// fakeNk implements the storage and notification calls the package makes; any
// other NakamaModule call panics on the nil embedded interface.
type fakeNk struct {
	runtime.NakamaModule
	mu            sync.Mutex
	objects       map[string]*api.StorageObject // keyed by collection/key
	writes        []*runtime.StorageWrite
	lists         []string // userIDs passed to StorageList
	notifications []*runtime.NotificationSend
	notifyErr     error
	pageSize      int
}

func newFakeNk() *fakeNk { return &fakeNk{objects: map[string]*api.StorageObject{}} }

func (n *fakeNk) StorageWrite(ctx context.Context, writes []*runtime.StorageWrite) ([]*api.StorageObjectAck, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, w := range writes {
		n.writes = append(n.writes, w)
		n.objects[w.Collection+"/"+w.Key] = &api.StorageObject{Collection: w.Collection, Key: w.Key, UserId: w.UserID, Value: w.Value}
	}
	return nil, nil
}

func (n *fakeNk) StorageRead(ctx context.Context, reads []*runtime.StorageRead) ([]*api.StorageObject, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []*api.StorageObject
	for _, r := range reads {
		if o, ok := n.objects[r.Collection+"/"+r.Key]; ok {
			out = append(out, o)
		}
	}
	return out, nil
}

func (n *fakeNk) StorageDelete(ctx context.Context, deletes []*runtime.StorageDelete) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, d := range deletes {
		delete(n.objects, d.Collection+"/"+d.Key)
	}
	return nil
}

// StorageList pages through a collection in key order; the cursor is the
// index of the next object.
func (n *fakeNk) StorageList(ctx context.Context, callerID, userID, collection string, limit int, cursor string) ([]*api.StorageObject, string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lists = append(n.lists, userID)
	var all []*api.StorageObject
	for _, o := range n.objects {
		if o.Collection == collection {
			all = append(all, o)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key < all[j].Key })
	if n.pageSize > 0 && n.pageSize < limit {
		limit = n.pageSize
	}
	start := 0
	if cursor != "" {
		fmt.Sscan(cursor, &start)
	}
	end := start + limit
	if end >= len(all) {
		return all[start:], "", nil
	}
	return all[start:end], fmt.Sprint(end), nil
}

func (n *fakeNk) NotificationsSend(ctx context.Context, notifications []*runtime.NotificationSend) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.notifyErr != nil {
		return n.notifyErr
	}
	n.notifications = append(n.notifications, notifications...)
	return nil
}

// --- logger and helpers ----------------------------------------------------

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

// newTestFleetManager wires a fleet manager to the fakes. The reaper is off
// unless cfg.ReapInterval is set; it stops when the test ends.
func newTestFleetManager(t *testing.T, cfg GameyeConfig, apiClient gameye.ApiClient) (*GameyeFleetManager, *fakeStorage, *fakeCallbackHandler) {
	t.Helper()
	if cfg.ReapInterval == 0 {
		cfg.ReapInterval = -1
	}
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	store := newFakeStorage()
	handler := newFakeCallbackHandler()
	fm := &GameyeFleetManager{ctx: t.Context(), config: cfg, logger: nopLogger{}, apiClient: apiClient, storage: store}
	if err := fm.Init(newFakeNk(), handler); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return fm, store, handler
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
