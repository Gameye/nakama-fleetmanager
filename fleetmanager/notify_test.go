package fleetmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

func testInstance() *runtime.InstanceInfo {
	return &runtime.InstanceInfo{Id: "session-1", ConnectionInfo: &runtime.ConnectionInfo{IpAddress: "1.2.3.4", Port: 21000}}
}

func TestNotifyConnectionInfoSendsPerUserExtras(t *testing.T) {
	nk := newFakeNk()
	extras := func(userId string) map[string]any {
		return map[string]any{"token": "token-for-" + userId}
	}

	if err := NotifyConnectionInfo(context.Background(), nk, []string{"u1", "u2"}, testInstance(), extras, true); err != nil {
		t.Fatalf("NotifyConnectionInfo: %v", err)
	}
	if len(nk.notifications) != 2 {
		t.Fatalf("notifications = %d, want one per user", len(nk.notifications))
	}
	for i, user := range []string{"u1", "u2"} {
		n := nk.notifications[i]
		if n.UserID != user || n.Subject != NotificationSubject || n.Code != NotificationCode || !n.Persistent || n.Sender != "" {
			t.Fatalf("notification %d = %+v", i, n)
		}
		c := n.Content
		if c[NotificationKeyHost] != "1.2.3.4" || c[NotificationKeyPort] != 21000 || c[NotificationKeySessionId] != "session-1" {
			t.Fatalf("content %d = %v", i, c)
		}
		if c["token"] != "token-for-"+user {
			t.Fatalf("user %s got extras %v", user, c)
		}
	}
	if nk.notifications[0].Content["token"] == nk.notifications[1].Content["token"] {
		t.Fatalf("both users received the same extras")
	}
}

func TestNotifyConnectionInfoExtrasCannotOverrideConnection(t *testing.T) {
	nk := newFakeNk()
	extras := func(string) map[string]any {
		return map[string]any{NotificationKeyHost: "evil", NotificationKeyPort: 1, NotificationKeySessionId: "x", "seat": 2}
	}
	if err := NotifyConnectionInfo(context.Background(), nk, []string{"u1"}, testInstance(), extras, false); err != nil {
		t.Fatalf("NotifyConnectionInfo: %v", err)
	}
	c := nk.notifications[0].Content
	if c[NotificationKeyHost] != "1.2.3.4" || c[NotificationKeyPort] != 21000 || c[NotificationKeySessionId] != "session-1" || c["seat"] != 2 {
		t.Fatalf("content = %v", c)
	}
	if nk.notifications[0].Persistent {
		t.Fatalf("persistent = true, want false")
	}
}

func TestNotifyConnectionInfoNilExtras(t *testing.T) {
	nk := newFakeNk()
	if err := NotifyConnectionInfo(context.Background(), nk, []string{"u1"}, testInstance(), nil, false); err != nil {
		t.Fatalf("NotifyConnectionInfo: %v", err)
	}
	if len(nk.notifications[0].Content) != 3 {
		t.Fatalf("content = %v, want host, port and session id only", nk.notifications[0].Content)
	}
}

func TestNotifyConnectionInfoNoUsersIsNoop(t *testing.T) {
	nk := newFakeNk()
	nk.notifyErr = errors.New("must not be called")
	if err := NotifyConnectionInfo(context.Background(), nk, nil, testInstance(), nil, false); err != nil {
		t.Fatalf("NotifyConnectionInfo: %v", err)
	}
}

func TestNotifyConnectionInfoErrors(t *testing.T) {
	nk := newFakeNk()
	if err := NotifyConnectionInfo(context.Background(), nk, []string{"u1"}, nil, nil, false); err == nil {
		t.Fatalf("expected an error for a nil instance")
	}
	if err := NotifyConnectionInfo(context.Background(), nk, []string{"u1"}, &runtime.InstanceInfo{Id: "a"}, nil, false); err == nil {
		t.Fatalf("expected an error for an instance without connection info")
	}
	nk.notifyErr = errors.New("boom")
	if err := NotifyConnectionInfo(context.Background(), nk, []string{"u1"}, testInstance(), nil, false); !errors.Is(err, nk.notifyErr) {
		t.Fatalf("err = %v, want the NotificationsSend error", err)
	}
}
