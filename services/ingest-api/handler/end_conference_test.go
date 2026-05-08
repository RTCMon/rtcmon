package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
	"github.com/RTCMon/rtcmon/services/ingest-api/handler"
)

const endConfSecret = "end-conf-test-secret"

// ── Infrastructure ────────────────────────────────────────────────────────────

func endConfDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DB_URL")
	if url == "" {
		t.Skip("TEST_DB_URL not set; skipping integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	endConfMigrate(t, pool)
	return pool
}

func endConfMigrate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, f := range []string{
		"../../../migrations/000001_initial_schema.up.sql",
		"../../../migrations/000002_add_external_id.up.sql",
	} {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Logf("migration %s: %v (may be harmless on existing schema)", f, err)
		}
	}
}

func endConfLog() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

// seedConference inserts org → app → conference and returns the numeric app ID.
func seedConference(t *testing.T, pool *pgxpool.Pool, confExternalID string) int64 {
	t.Helper()
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('end-conf-org') RETURNING id`,
	).Scan(&orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}

	var appID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'end-conf-app', 'hash') RETURNING id`,
		orgID,
	).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO conferences (app_id, external_id) VALUES ($1, $2)`,
		appID, confExternalID,
	); err != nil {
		t.Fatalf("seed conference: %v", err)
	}
	return appID
}

// signToken builds a valid HS256 JWT carrying the given appID.
func signToken(t *testing.T, appID string) string {
	t.Helper()
	claims := auth.Claims{}
	claims.AppID = appID
	claims.ConferenceID = "irrelevant"
	claims.UserID = "user-test"
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(time.Hour))
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(endConfSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

// newEndConfServer wraps HandleEndConference in a minimal chi router with the
// real auth.Authenticate middleware and returns an httptest.Server.
func newEndConfServer(t *testing.T, pool *pgxpool.Pool, trigger handler.EMOSTriggerFn) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(auth.Authenticate(endConfSecret))
		r.Post("/v1/conferences/{conferenceID}/end",
			handler.HandleEndConference(pool, endConfLog(), trigger))
	})
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

// doEndConf sends POST /v1/conferences/{extID}/end with the given JWT.
func doEndConf(t *testing.T, ts *httptest.Server, extID, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/v1/conferences/"+extID+"/end", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestEndConference_Valid verifies that a valid request returns 202 and sets
// ended_at on the conference row.
func TestEndConference_Valid(t *testing.T) {
	pool := endConfDB(t)
	const extID = "conf-end-valid"
	appID := seedConference(t, pool, extID)

	ts := newEndConfServer(t, pool, nil)
	resp := doEndConf(t, ts, extID, signToken(t, fmt.Sprintf("%d", appID)))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var endedAt *time.Time
	pool.QueryRow(context.Background(),
		`SELECT ended_at FROM conferences WHERE external_id = $1`, extID,
	).Scan(&endedAt)
	if endedAt == nil {
		t.Error("want ended_at to be set, got NULL")
	}
}

// TestEndConference_Idempotent calls the endpoint twice and verifies that
// ended_at is not overwritten on the second call.
func TestEndConference_Idempotent(t *testing.T) {
	pool := endConfDB(t)
	const extID = "conf-end-idempotent"
	appID := seedConference(t, pool, extID)
	token := signToken(t, fmt.Sprintf("%d", appID))

	ts := newEndConfServer(t, pool, nil)

	resp1 := doEndConf(t, ts, extID, token)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first call: want 202, got %d", resp1.StatusCode)
	}

	var firstEndedAt time.Time
	pool.QueryRow(context.Background(),
		`SELECT ended_at FROM conferences WHERE external_id = $1`, extID,
	).Scan(&firstEndedAt)

	time.Sleep(10 * time.Millisecond) // ensure now() would differ if re-run

	resp2 := doEndConf(t, ts, extID, token)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("second call: want 202, got %d", resp2.StatusCode)
	}

	var secondEndedAt time.Time
	pool.QueryRow(context.Background(),
		`SELECT ended_at FROM conferences WHERE external_id = $1`, extID,
	).Scan(&secondEndedAt)

	if !firstEndedAt.Equal(secondEndedAt) {
		t.Errorf("ended_at changed on second call: %v → %v", firstEndedAt, secondEndedAt)
	}
}

// TestEndConference_NotFound sends a request for a non-existent external_id
// and expects 404 with a JSON error body.
func TestEndConference_NotFound(t *testing.T) {
	pool := endConfDB(t)
	ts := newEndConfServer(t, pool, nil)

	resp := doEndConf(t, ts, "does-not-exist", signToken(t, "1"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["error"] == "" {
		t.Error("want non-empty error field in response body")
	}
}

// TestEndConference_CrossApp seeds a conference under appA and calls the
// endpoint with a JWT from appB, expecting 403.
func TestEndConference_CrossApp(t *testing.T) {
	pool := endConfDB(t)
	ctx := context.Background()

	const extID = "conf-end-crossapp"
	seedConference(t, pool, extID) // owned by appA

	// Seed a second independent app (appB).
	var orgBID int64
	pool.QueryRow(ctx,
		`INSERT INTO organizations (name) VALUES ('org-b') RETURNING id`,
	).Scan(&orgBID)
	var appBID int64
	pool.QueryRow(ctx,
		`INSERT INTO apps (org_id, name, api_key_hash) VALUES ($1, 'app-b', 'hash-b') RETURNING id`,
		orgBID,
	).Scan(&appBID)

	ts := newEndConfServer(t, pool, nil)
	resp := doEndConf(t, ts, extID, signToken(t, fmt.Sprintf("%d", appBID)))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d: body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// TestEndConference_eMOSTriggered verifies that the eMOS trigger function
// receives the conference DB id after a successful end.
func TestEndConference_eMOSTriggered(t *testing.T) {
	pool := endConfDB(t)
	ctx := context.Background()

	const extID = "conf-end-emos"
	appID := seedConference(t, pool, extID)

	triggered := make(chan int64, 1)
	trigger := func(id int64) { triggered <- id }

	ts := newEndConfServer(t, pool, trigger)
	resp := doEndConf(t, ts, extID, signToken(t, fmt.Sprintf("%d", appID)))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var confDBID int64
	pool.QueryRow(ctx,
		`SELECT id FROM conferences WHERE external_id = $1`, extID,
	).Scan(&confDBID)

	select {
	case got := <-triggered:
		if got != confDBID {
			t.Errorf("eMOS trigger: want confID=%d, got %d", confDBID, got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("eMOS trigger not called within 100ms")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
