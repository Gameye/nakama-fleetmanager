package gameye

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   []byte
}

type fakeServer struct {
	t        *testing.T
	mu       sync.Mutex
	requests []recordedRequest
	status   int
	body     string
}

func newFakeServer(t *testing.T, status int, body string) (*fakeServer, ApiClient) {
	t.Helper()
	fs := &fakeServer{t: t, status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fs.mu.Lock()
		fs.requests = append(fs.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Auth:   r.Header.Get("Authorization"),
			Body:   b,
		})
		fs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fs.status)
		_, _ = io.WriteString(w, fs.body)
	}))
	t.Cleanup(srv.Close)

	client, err := NewApiClient(srv.URL, "test-token")
	if err != nil {
		t.Fatalf("NewApiClient: %v", err)
	}
	return fs, client
}

func (fs *fakeServer) last() recordedRequest {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.requests) == 0 {
		fs.t.Fatalf("no request recorded")
	}
	return fs.requests[len(fs.requests)-1]
}

func errorBody(status int) string {
	return `{"statusCode":` + itoa(status) + `,"code":"x","message":"boom","details":"d","path":"/session","identifier":"i","timestamp":"t"}`
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestSessionRunSendsTtlExternalIdEnvAndVersion(t *testing.T) {
	fs, client := newFakeServer(t, http.StatusCreated, `{"id":"abc","host":"1.2.3.4","ports":[{"type":"tcp","container":7360,"host":21000}]}`)

	_, err := client.SessionRun(context.Background(), SessionRun{
		ID:         "abc",
		Region:     "europe",
		Image:      "my-game",
		Tag:        "1.0.0",
		Ttl:        "30m",
		ExternalID: "match-1",
		EnvVars:    map[string]string{"SEAT_SECRET": "x"},
		Labels:     map[string]string{"mode": "duel"},
	})
	if err != nil {
		t.Fatalf("SessionRun: %v", err)
	}

	req := fs.last()
	if req.Method != http.MethodPost || req.Path != "/session" {
		t.Fatalf("unexpected request %s %s", req.Method, req.Path)
	}
	if req.Auth != "Bearer test-token" {
		t.Fatalf("unexpected auth header %q", req.Auth)
	}

	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	want := map[string]any{
		"id":          "abc",
		"location":    "europe",
		"image":       "my-game",
		"version":     "1.0.0",
		"ttl":         "30m",
		"external_id": "match-1",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, body[k], v)
		}
	}
	env, _ := body["env"].(map[string]any)
	if env["SEAT_SECRET"] != "x" {
		t.Errorf("env not sent: %v", body["env"])
	}
	labels, _ := body["labels"].(map[string]any)
	if _, leaked := labels["SEAT_SECRET"]; leaked {
		t.Errorf("env leaked into labels: %v", labels)
	}
}

func TestSessionRunOmitsEmptyOptionalFields(t *testing.T) {
	fs, client := newFakeServer(t, http.StatusCreated, `{"id":"abc","host":"1.2.3.4","ports":[]}`)

	if _, err := client.SessionRun(context.Background(), SessionRun{ID: "abc", Region: "europe", Image: "g"}); err != nil {
		t.Fatalf("SessionRun: %v", err)
	}

	var body map[string]any
	_ = json.Unmarshal(fs.last().Body, &body)
	// env values and args items have minLength 1 in the spec; empty
	// collections and empty strings must not be sent.
	for _, k := range []string{"env", "args", "ttl", "external_id", "version"} {
		if _, ok := body[k]; ok {
			t.Errorf("expected %q to be omitted, body=%s", k, fs.last().Body)
		}
	}
}

func TestSessionRunWithoutResponseIdUsesRequestedId(t *testing.T) {
	_, client := newFakeServer(t, http.StatusCreated, `{"host":"1.2.3.4","ports":[{"type":"udp","container":7360,"host":21000}]}`)

	started, err := client.SessionRun(context.Background(), SessionRun{ID: "requested", Region: "europe", Image: "g"})
	if err != nil {
		t.Fatalf("SessionRun: %v", err)
	}
	if started.ID != "requested" {
		t.Fatalf("ID = %q, want requested", started.ID)
	}
}

func TestSessionRunPortsAreKeyedByContainerPort(t *testing.T) {
	_, client := newFakeServer(t, http.StatusCreated, `{"id":"abc","host":"1.2.3.4","ports":[{"type":"tcp","container":9000,"host":20001},{"type":"tcp","container":7360,"host":20002}]}`)

	started, err := client.SessionRun(context.Background(), SessionRun{ID: "abc", Region: "europe", Image: "g"})
	if err != nil {
		t.Fatalf("SessionRun: %v", err)
	}

	port, ok := HostPort(started.PortMap(), "7360/tcp")
	if !ok || port != 20002 {
		t.Fatalf("HostPort(7360/tcp) = %d, %v; want 20002, true", port, ok)
	}
}

func TestHostPortIsDeterministic(t *testing.T) {
	ports := map[string]int{"9000/udp": 3, "7360/udp": 2, "7360/tcp": 1, "10000/tcp": 4}

	if p, ok := HostPort(ports, "9000/udp"); !ok || p != 3 {
		t.Fatalf("configured key: got %d, %v", p, ok)
	}
	if _, ok := HostPort(ports, "1234/tcp"); ok {
		t.Fatalf("missing configured key must report not found")
	}
	// Without a configured key, the lowest container port wins, tcp before udp.
	for i := 0; i < 50; i++ {
		if p, ok := HostPort(ports, ""); !ok || p != 1 {
			t.Fatalf("unconfigured: got %d, %v; want 1, true", p, ok)
		}
	}
	if _, ok := HostPort(nil, ""); ok {
		t.Fatalf("no ports must report not found")
	}
}

func TestSessionRunErrorMapping(t *testing.T) {
	cases := []struct {
		status    int
		sentinel  error
		retryable bool
	}{
		{http.StatusUnauthorized, ErrUnauthorized, false},
		{http.StatusPaymentRequired, ErrQuotaExceeded, false},
		{http.StatusForbidden, ErrForbidden, false},
		{http.StatusNotFound, ErrNotFound, false},
		{420, ErrNoCapacity, false},
		{http.StatusInternalServerError, ErrInternalServer, true},
		{http.StatusBadGateway, ErrInternalServer, true},
		{http.StatusServiceUnavailable, ErrInternalServer, true},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status)+itoa(tc.status), func(t *testing.T) {
			_, client := newFakeServer(t, tc.status, errorBody(tc.status))

			_, err := client.SessionRun(context.Background(), SessionRun{ID: "abc", Region: "europe", Image: "g"})
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.sentinel)
			}
			if got := IsRetryable(err); got != tc.retryable {
				t.Fatalf("IsRetryable = %v, want %v", got, tc.retryable)
			}
			var apiErr *ApiError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status {
				t.Fatalf("expected *ApiError with status %d, got %#v", tc.status, err)
			}
		})
	}
}

func TestErrRanOutOfComputeAliasesNoCapacity(t *testing.T) {
	_, client := newFakeServer(t, 420, errorBody(420))
	_, err := client.SessionRun(context.Background(), SessionRun{ID: "abc", Region: "europe", Image: "g"})
	if !errors.Is(err, ErrRanOutOfCompute) {
		t.Fatalf("420 should still match the legacy ErrRanOutOfCompute, got %v", err)
	}
}

func TestNonJsonErrorBodyKeepsStatus(t *testing.T) {
	_, client := newFakeServer(t, http.StatusBadGateway, `<html>bad gateway</html>`)

	_, err := client.SessionRun(context.Background(), SessionRun{ID: "abc", Region: "europe", Image: "g"})
	if !errors.Is(err, ErrInternalServer) || !IsRetryable(err) {
		t.Fatalf("502 with html body must be a retryable server error, got %v", err)
	}
}

func TestSessionStopTreatsGoneAsSuccess(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotFound, http.StatusConflict} {
		t.Run(itoa(status), func(t *testing.T) {
			fs, client := newFakeServer(t, status, errorBody(status))
			if err := client.SessionStop(context.Background(), SessionStop{ID: "abc"}); err != nil {
				t.Fatalf("SessionStop with %d: %v", status, err)
			}
			if r := fs.last(); r.Method != http.MethodDelete || r.Path != "/session/abc" {
				t.Fatalf("unexpected request %s %s", r.Method, r.Path)
			}
		})
	}
}

func TestSessionStopForbiddenIsAnError(t *testing.T) {
	_, client := newFakeServer(t, http.StatusForbidden, errorBody(http.StatusForbidden))
	if err := client.SessionStop(context.Background(), SessionStop{ID: "abc"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

func TestSessionListMissingPlayerCountDoesNotPanic(t *testing.T) {
	_, client := newFakeServer(t, http.StatusOK, `{"sessions":[{"id":"a","image":"g","location":"europe","host":"1.2.3.4","created":1648472895123,"port":{"9000/tcp":20001,"7360/tcp":20002},"status":"running"}]}`)

	list, err := client.SessionList(context.Background(), SessionList{Region: "europe"})
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d", len(list))
	}
	got := list[0]
	if got.PlayerCount != 0 {
		t.Errorf("PlayerCount = %d, want 0", got.PlayerCount)
	}
	if got.Created.UnixMilli() != 1648472895123 {
		t.Errorf("Created = %d ms, want 1648472895123 (no precision loss)", got.Created.UnixMilli())
	}
	if p, ok := HostPort(got.Ports, "7360/tcp"); !ok || p != 20002 {
		t.Errorf("HostPort(7360/tcp) = %d, %v", p, ok)
	}
}

func TestSessionListPassesFilters(t *testing.T) {
	fs, client := newFakeServer(t, http.StatusOK, `{"sessions":[]}`)
	if _, err := client.SessionList(context.Background(), SessionList{Region: "europe", Image: "g", Tag: "v1"}); err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	r := fs.last()
	if r.Path != "/session" {
		t.Fatalf("path = %s", r.Path)
	}
	for _, want := range []string{"location=europe", "image=g", "tag=v1"} {
		if !strings.Contains(r.Query, want) {
			t.Errorf("query %q missing %q", r.Query, want)
		}
	}
}

func TestSessionDescribe(t *testing.T) {
	_, client := newFakeServer(t, http.StatusOK, `{"id":"a","image":"g","tag":"v1","location":"europe","host":"1.2.3.4","created":1648472895123,"port":{"7360/udp":20005,"9000/tcp":20001},"status":"running","labels":{},"players":{"joined":["u1","u2"],"joinedCount":2},"playerCount":null}`)

	s, err := client.SessionDescribe(context.Background(), SessionDescribe{ID: "a"})
	if err != nil {
		t.Fatalf("SessionDescribe: %v", err)
	}
	if s.PlayerCount != 2 || s.IPV4Address != "1.2.3.4" || s.Created.UnixMilli() != 1648472895123 {
		t.Fatalf("unexpected session %+v", s)
	}
	if p, ok := HostPort(s.Ports, "7360/udp"); !ok || p != 20005 {
		t.Fatalf("HostPort(7360/udp) = %d, %v", p, ok)
	}
}

func TestSessionDescribeNotFound(t *testing.T) {
	_, client := newFakeServer(t, http.StatusNotFound, errorBody(http.StatusNotFound))
	if _, err := client.SessionDescribe(context.Background(), SessionDescribe{ID: "a"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSessionJoin(t *testing.T) {
	fs, client := newFakeServer(t, http.StatusOK, `{"count":2,"players":["u1","u2"]}`)
	players, err := client.SessionJoin(context.Background(), SessionJoin{ID: "a", PlayerIDs: []string{"u1", "u2"}})
	if err != nil {
		t.Fatalf("SessionJoin: %v", err)
	}
	if len(players) != 2 {
		t.Fatalf("players = %v", players)
	}
	r := fs.last()
	if r.Method != http.MethodPut || r.Path != "/session/player/join" {
		t.Fatalf("unexpected request %s %s", r.Method, r.Path)
	}
}

func TestSessionListAndDescribeReturnLabels(t *testing.T) {
	_, client := newFakeServer(t, http.StatusOK, `{"sessions":[{"id":"a","image":"g","location":"europe","host":"1.2.3.4","created":1648472895123,"port":{"7360/tcp":20002},"status":"running","labels":{"mode":"duel"}},{"id":"b","image":"g","location":"europe","host":"1.2.3.4","created":1648472895123,"port":{},"status":"running"}]}`)
	sessions, err := client.SessionList(context.Background(), SessionList{})
	if err != nil {
		t.Fatalf("SessionList: %v", err)
	}
	if sessions[0].Labels["mode"] != "duel" {
		t.Fatalf("labels = %v", sessions[0].Labels)
	}
	if sessions[1].Labels != nil {
		t.Fatalf("missing labels should be nil, got %v", sessions[1].Labels)
	}

	_, client = newFakeServer(t, http.StatusOK, `{"id":"a","image":"g","tag":"v1","location":"europe","host":"1.2.3.4","created":1648472895123,"port":{},"status":"running","labels":{"mode":"duel"},"players":{"joined":[],"joinedCount":0}}`)
	session, err := client.SessionDescribe(context.Background(), SessionDescribe{ID: "a"})
	if err != nil {
		t.Fatalf("SessionDescribe: %v", err)
	}
	if session.Labels["mode"] != "duel" {
		t.Fatalf("labels = %v", session.Labels)
	}
}
