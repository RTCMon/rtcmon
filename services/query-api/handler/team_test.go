package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/handler"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func setupTeamCleanup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		TRUNCATE invitations, organization_members, apps, organizations, users RESTART IDENTITY CASCADE
	`)
	if err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			TRUNCATE invitations, organization_members, apps, organizations, users RESTART IDENTITY CASCADE
		`)
	})
	return pool
}

func createTestOrg(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO organizations (name) VALUES ('Test Org')
		RETURNING id
	`).Scan(&orgID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return orgID
}

func createTestUser(t *testing.T, pool *pgxpool.Pool, email, name, password string) int64 {
	t.Helper()
	ctx := context.Background()
	hash := password // For testing, just use the password as-is
	var userID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO users (email, name, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id
	`, email, name, hash).Scan(&userID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return userID
}

func addUserToOrg(t *testing.T, pool *pgxpool.Pool, orgID, userID int64, role string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO organization_members (org_id, user_id, role, invited_by)
		VALUES ($1, $2, $3, NULL)
	`, orgID, userID, role)
	if err != nil {
		t.Fatalf("add user to org: %v", err)
	}
}

func withChiParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// ─── Tests: ListMembers ──────────────────────────────────────────────────────

func TestListMembers_Success(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	user1 := createTestUser(t, pool, "alice@test.com", "Alice", "pass123456")
	user2 := createTestUser(t, pool, "bob@test.com", "Bob", "pass123456")
	addUserToOrg(t, pool, orgID, user1, "admin")
	addUserToOrg(t, pool, orgID, user2, "member")

	sess := &session.Data{UserID: user1, OrgID: orgID, Role: "admin", Email: "alice@test.com", Name: "Alice"}
	ctx := session.WithContext(context.Background(), sess)
	req := httptest.NewRequest(http.MethodGet, "/v1/team/members", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	h := handler.HandleListMembers(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusOK)
	body := bodyJSON(t, w)
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("data is not array: %v", body["data"])
	}
	if len(data) != 2 {
		t.Errorf("member count: got %d, want 2", len(data))
	}
}

func TestListMembers_Unauthenticated(t *testing.T) {
	pool := setupTeamCleanup(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/team/members", nil)
	w := httptest.NewRecorder()

	h := handler.HandleListMembers(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusUnauthorized)
}

func TestListMembers_EmptyOrg(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	user1 := createTestUser(t, pool, "alice@test.com", "Alice", "pass123456")
	addUserToOrg(t, pool, orgID, user1, "admin")

	sess := &session.Data{UserID: user1, OrgID: 99, Role: "admin"} // different org
	ctx := session.WithContext(context.Background(), sess)
	req := httptest.NewRequest(http.MethodGet, "/v1/team/members", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	h := handler.HandleListMembers(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusOK)
	body := bodyJSON(t, w)
	data := body["data"]
	if data == nil {
		t.Errorf("data is nil")
	}
}

// ─── Tests: ChangeRole ───────────────────────────────────────────────────────

func TestChangeRole_AdminOnly(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	member := createTestUser(t, pool, "member@test.com", "Member", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")
	addUserToOrg(t, pool, orgID, member, "member")

	// Member tries to change role
	sess := &session.Data{UserID: member, OrgID: orgID, Role: "member"}
	ctx := session.WithContext(context.Background(), sess)
	body := map[string]string{"role": "admin"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPatch, "/", bytes.NewReader(bodyBytes)).WithContext(ctx)
	req = withChiParam(req, "userId", "1")
	w := httptest.NewRecorder()

	h := handler.HandleChangeRole(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusForbidden)
}

func TestChangeRole_Success(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	member := createTestUser(t, pool, "member@test.com", "Member", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")
	addUserToOrg(t, pool, orgID, member, "member")

	sess := &session.Data{UserID: admin, OrgID: orgID, Role: "admin"}
	ctx := session.WithContext(context.Background(), sess)
	body := map[string]string{"role": "admin"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPatch, "/", bytes.NewReader(bodyBytes)).WithContext(ctx)
	req = withChiParam(req, "userId", "2")
	w := httptest.NewRecorder()

	h := handler.HandleChangeRole(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusOK)
	respBody := bodyJSON(t, w)
	if respBody["data"] == nil {
		t.Fatalf("data is nil")
	}
}

func TestChangeRole_InvalidRole(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	member := createTestUser(t, pool, "member@test.com", "Member", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")
	addUserToOrg(t, pool, orgID, member, "member")

	sess := &session.Data{UserID: admin, OrgID: orgID, Role: "admin"}
	ctx := session.WithContext(context.Background(), sess)
	body := map[string]string{"role": "invalid"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPatch, "/", bytes.NewReader(bodyBytes)).WithContext(ctx)
	req = withChiParam(req, "userId", "2")
	w := httptest.NewRecorder()

	h := handler.HandleChangeRole(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusBadRequest)
}

func TestChangeRole_LastAdminDemotion(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")

	sess := &session.Data{UserID: admin, OrgID: orgID, Role: "admin"}
	ctx := session.WithContext(context.Background(), sess)
	body := map[string]string{"role": "member"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPatch, "/", bytes.NewReader(bodyBytes)).WithContext(ctx)
	req = withChiParam(req, "userId", "1")
	w := httptest.NewRecorder()

	h := handler.HandleChangeRole(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusConflict)
	resp := bodyJSON(t, w)
	if e, ok := resp["error"].(string); !ok || e != "cannot remove last admin" {
		t.Errorf("expected 'cannot remove last admin' error, got: %v", resp["error"])
	}
}

// ─── Tests: RemoveMember ─────────────────────────────────────────────────────

func TestRemoveMember_AdminOnly(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	member := createTestUser(t, pool, "member@test.com", "Member", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")
	addUserToOrg(t, pool, orgID, member, "member")

	sess := &session.Data{UserID: member, OrgID: orgID, Role: "member"}
	ctx := session.WithContext(context.Background(), sess)
	req := httptest.NewRequest(http.MethodDelete, "/", nil).WithContext(ctx)
	req = withChiParam(req, "userId", "2")
	w := httptest.NewRecorder()

	h := handler.HandleRemoveMember(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusForbidden)
}

func TestRemoveMember_Success(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	member := createTestUser(t, pool, "member@test.com", "Member", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")
	addUserToOrg(t, pool, orgID, member, "member")

	sess := &session.Data{UserID: admin, OrgID: orgID, Role: "admin"}
	ctx := session.WithContext(context.Background(), sess)
	req := httptest.NewRequest(http.MethodDelete, "/", nil).WithContext(ctx)
	req = withChiParam(req, "userId", "2")
	w := httptest.NewRecorder()

	h := handler.HandleRemoveMember(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusOK)
}

func TestRemoveMember_LastAdmin(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")

	sess := &session.Data{UserID: admin, OrgID: orgID, Role: "admin"}
	ctx := session.WithContext(context.Background(), sess)
	req := httptest.NewRequest(http.MethodDelete, "/", nil).WithContext(ctx)
	req = withChiParam(req, "userId", "1")
	w := httptest.NewRecorder()

	h := handler.HandleRemoveMember(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusConflict)
}

// ─── Tests: CreateInvitation ─────────────────────────────────────────────────

func TestCreateInvitation_Success(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")

	sess := &session.Data{UserID: admin, OrgID: orgID, Role: "admin"}
	ctx := session.WithContext(context.Background(), sess)
	body := map[string]string{"email": "newuser@test.com", "role": "member"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bodyBytes)).WithContext(ctx)
	w := httptest.NewRecorder()

	h := handler.HandleCreateInvitation(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusCreated)
	respBody := bodyJSON(t, w)
	if respBody["data"] == nil {
		t.Fatalf("data is nil")
	}
}

func TestCreateInvitation_DuplicateEmail(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	member := createTestUser(t, pool, "member@test.com", "Member", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")
	addUserToOrg(t, pool, orgID, member, "member")

	sess := &session.Data{UserID: admin, OrgID: orgID, Role: "admin"}
	ctx := session.WithContext(context.Background(), sess)
	body := map[string]string{"email": "member@test.com", "role": "member"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bodyBytes)).WithContext(ctx)
	w := httptest.NewRecorder()

	h := handler.HandleCreateInvitation(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusConflict)
}

// ─── Tests: ListInvitations ──────────────────────────────────────────────────

func TestListInvitations_PendingOnly(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")

	// Create three invitations via DB: one pending, one expired, one accepted
	ctx := context.Background()
	now := time.Now().UTC()

	// Pending (7 days from now)
	_, _ = pool.Exec(ctx, `
		INSERT INTO invitations (token, org_id, email, role, expires_at)
		VALUES ('token1', $1, 'pending@test.com', 'member', $2)
	`, orgID, now.Add(7*24*time.Hour))

	// Expired (1 day ago)
	_, _ = pool.Exec(ctx, `
		INSERT INTO invitations (token, org_id, email, role, expires_at)
		VALUES ('token2', $1, 'expired@test.com', 'member', $2)
	`, orgID, now.Add(-24*time.Hour))

	// Accepted
	_, _ = pool.Exec(ctx, `
		INSERT INTO invitations (token, org_id, email, role, expires_at, accepted_at)
		VALUES ('token3', $1, 'accepted@test.com', 'member', $2, $3)
	`, orgID, now.Add(7*24*time.Hour), now)

	sess := &session.Data{UserID: admin, OrgID: orgID, Role: "admin"}
	ctx = session.WithContext(context.Background(), sess)
	req := httptest.NewRequest(http.MethodGet, "/v1/team/invitations", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	h := handler.HandleListInvitations(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusOK)
	respBody := bodyJSON(t, w)
	data, ok := respBody["data"].([]any)
	if !ok {
		t.Fatalf("data is not array")
	}
	if len(data) != 1 {
		t.Errorf("expected 1 pending invitation, got %d", len(data))
	}
}

// ─── Tests: RevokeInvitation ─────────────────────────────────────────────────

func TestRevokeInvitation_Success(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)
	admin := createTestUser(t, pool, "admin@test.com", "Admin", "pass123456")
	addUserToOrg(t, pool, orgID, admin, "admin")

	// Create invitation
	now := time.Now().UTC()
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO invitations (token, org_id, email, role, expires_at)
		VALUES ('test-token', $1, 'invite@test.com', 'member', $2)
	`, orgID, now.Add(7*24*time.Hour))

	sess := &session.Data{UserID: admin, OrgID: orgID, Role: "admin"}
	ctx := session.WithContext(context.Background(), sess)
	req := httptest.NewRequest(http.MethodDelete, "/", nil).WithContext(ctx)
	req = withChiParam(req, "token", "test-token")
	w := httptest.NewRecorder()

	h := handler.HandleRevokeInvitation(pool, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusOK)
}

// ─── Tests: AcceptInvitation ─────────────────────────────────────────────────

func TestAcceptInvitation_Expired(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)

	// Create expired invitation
	now := time.Now().UTC()
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO invitations (token, org_id, email, role, expires_at)
		VALUES ('expired-token', $1, 'user@test.com', 'member', $2)
	`, orgID, now.Add(-24*time.Hour)) // Expired 1 day ago

	body := map[string]string{"name": "New User", "password": "SecurePass123"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bodyBytes))
	req = withChiParam(req, "token", "expired-token")
	w := httptest.NewRecorder()

	_, sessions := newMiniredisStore(t)
	h := handler.HandleAcceptInvitation(pool, sessions, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusGone)
}

func TestAcceptInvitation_AlreadyAccepted(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)

	// Create already-accepted invitation
	now := time.Now().UTC()
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO invitations (token, org_id, email, role, expires_at, accepted_at)
		VALUES ('accepted-token', $1, 'user@test.com', 'member', $2, $3)
	`, orgID, now.Add(7*24*time.Hour), now)

	body := map[string]string{"name": "New User", "password": "SecurePass123"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bodyBytes))
	req = withChiParam(req, "token", "accepted-token")
	w := httptest.NewRecorder()

	_, sessions := newMiniredisStore(t)
	h := handler.HandleAcceptInvitation(pool, sessions, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusConflict)
}

func TestAcceptInvitation_NotFound(t *testing.T) {
	pool := setupTeamCleanup(t)

	body := map[string]string{"name": "New User", "password": "SecurePass123"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bodyBytes))
	req = withChiParam(req, "token", "nonexistent-token")
	w := httptest.NewRecorder()

	_, sessions := newMiniredisStore(t)
	h := handler.HandleAcceptInvitation(pool, sessions, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusNotFound)
}

func TestAcceptInvitation_WeakPassword(t *testing.T) {
	pool := setupTeamCleanup(t)
	orgID := createTestOrg(t, pool)

	// Create valid invitation
	now := time.Now().UTC()
	_, _ = pool.Exec(context.Background(), `
		INSERT INTO invitations (token, org_id, email, role, expires_at)
		VALUES ('valid-token', $1, 'user@test.com', 'member', $2)
	`, orgID, now.Add(7*24*time.Hour))

	body := map[string]string{"name": "New User", "password": "weak"}
	bodyBytes, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bodyBytes))
	req = withChiParam(req, "token", "valid-token")
	w := httptest.NewRecorder()

	_, sessions := newMiniredisStore(t)
	h := handler.HandleAcceptInvitation(pool, sessions, noopLog())
	h(w, req)

	requireStatus(t, w, http.StatusBadRequest)
}
