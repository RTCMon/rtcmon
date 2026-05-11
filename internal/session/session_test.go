package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/RTCMon/rtcmon/internal/session"
)

func newTestStore(t *testing.T) *session.Store {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return session.NewStore(rdb, 3600)
}

func TestSession_CreateAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	data := session.Data{UserID: 42, OrgID: 7, Role: "admin", Email: "a@b.com", Name: "Alice"}
	token, err := store.Create(ctx, data)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(token) != 64 {
		t.Errorf("token length: want 64, got %d", len(token))
	}

	got, err := store.Get(ctx, token)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get: returned nil for valid token")
	}
	if got.UserID != data.UserID || got.OrgID != data.OrgID || got.Role != data.Role {
		t.Errorf("Get: got %+v, want %+v", got, data)
	}
}

func TestSession_GetMissing(t *testing.T) {
	store := newTestStore(t)
	got, err := store.Get(context.Background(), "nonexistent-token")
	if err != nil {
		t.Fatalf("Get missing: unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("Get missing: want nil, got %+v", got)
	}
}

func TestSession_Delete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	token, _ := store.Create(ctx, session.Data{UserID: 1, OrgID: 1, Role: "member"})
	if err := store.Delete(ctx, token); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ := store.Get(ctx, token)
	if got != nil {
		t.Error("Delete: session still exists after deletion")
	}
}

func TestSession_DeleteNonExistent(t *testing.T) {
	store := newTestStore(t)
	if err := store.Delete(context.Background(), "no-such-token"); err != nil {
		t.Errorf("Delete non-existent: want nil error, got %v", err)
	}
}

func TestSession_TTLRespected(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	store := session.NewStore(rdb, 1) // 1-second TTL

	ctx := context.Background()
	token, _ := store.Create(ctx, session.Data{UserID: 1, OrgID: 1, Role: "admin"})

	mr.FastForward(2 * time.Second)

	got, err := store.Get(ctx, token)
	if err != nil {
		t.Fatalf("Get after expiry: %v", err)
	}
	if got != nil {
		t.Error("Get after expiry: want nil (expired), got session data")
	}
}

func TestContext_WithAndFrom(t *testing.T) {
	data := &session.Data{UserID: 5, OrgID: 3, Role: "admin"}
	ctx := session.WithContext(context.Background(), data)

	got := session.FromContext(ctx)
	if got == nil {
		t.Fatal("FromContext: returned nil")
	}
	if got.UserID != data.UserID {
		t.Errorf("FromContext: UserID = %d, want %d", got.UserID, data.UserID)
	}
}

func TestContext_FromEmpty(t *testing.T) {
	got := session.FromContext(context.Background())
	if got != nil {
		t.Errorf("FromContext on empty context: want nil, got %+v", got)
	}
}
