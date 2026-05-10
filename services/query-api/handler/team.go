package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"

	"github.com/RTCMon/rtcmon/internal/session"
)

// ─── Types ───────────────────────────────────────────────────────────────────

// MemberItem represents a team member in list responses.
type MemberItem struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Email    string    `json:"email"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// InvitationItem represents a pending invitation in list responses.
type InvitationItem struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ChangeRoleRequest is the body for PATCH /v1/team/members/:userId/role.
type ChangeRoleRequest struct {
	Role string `json:"role"`
}

// CreateInvitationRequest is the body for POST /v1/team/invitations.
type CreateInvitationRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// AcceptInvitationRequest is the body for POST /v1/team/invitations/:token/accept.
type AcceptInvitationRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// ─── Handlers ────────────────────────────────────────────────────────────────

// HandleListMembers returns a handler for GET /v1/team/members.
// Lists all organization members with their roles.
// Auth: Session required (all roles can read).
// @Summary List Members
// @Description List Members endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/team/members [get]
func HandleListMembers(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		rows, err := db.Query(r.Context(), `
			SELECT om.user_id, u.name, u.email, om.role, om.joined_at
			FROM organization_members om
			JOIN users u ON om.user_id = u.id
			WHERE om.org_id = $1
			ORDER BY om.joined_at ASC
		`, sess.OrgID)
		if err != nil {
			log.WithError(err).Error("list members: query")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer rows.Close()

		var members []MemberItem
		for rows.Next() {
			var m MemberItem
			if err := rows.Scan(&m.ID, &m.Name, &m.Email, &m.Role, &m.JoinedAt); err != nil {
				log.WithError(err).Error("list members: scan")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			members = append(members, m)
		}
		if members == nil {
			members = []MemberItem{} // return empty array, not null
		}

		writeJSON(w, http.StatusOK, map[string]any{"data": members})
	}
}

// HandleChangeRole returns a handler for PATCH /v1/team/members/:userId/role.
// Changes a member's role. Admin-only. Cannot demote the last admin.
// @Summary Change Role
// @Description Change Role endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/team/members/{userId}/role [patch]
func HandleChangeRole(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if !requireAdmin(w, sess) {
			return
		}

		userID, err := strconv.ParseInt(chi.URLParam(r, "userId"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid userId"})
			return
		}

		var req ChangeRoleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}

		// Validate role is one of the known values.
		if req.Role != "admin" && req.Role != "member" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be 'admin' or 'member'"})
			return
		}

		tx, err := db.Begin(r.Context())
		if err != nil {
			log.WithError(err).Error("change role: begin tx")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer tx.Rollback(r.Context()) //nolint:errcheck

		// Get current role
		var currentRole string
		err = tx.QueryRow(r.Context(), `
			SELECT role FROM organization_members
			WHERE org_id = $1 AND user_id = $2
		`, sess.OrgID, userID).Scan(&currentRole)
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "member not found"})
			return
		}
		if err != nil {
			log.WithError(err).Error("change role: query current role")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// If demoting from admin to member, check if this is the last admin
		if currentRole == "admin" && req.Role == "member" {
			var adminCount int64
			err = tx.QueryRow(r.Context(), `
				SELECT COUNT(*) FROM organization_members
				WHERE org_id = $1 AND role = 'admin'
			`, sess.OrgID).Scan(&adminCount)
			if err != nil {
				log.WithError(err).Error("change role: count admins")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			if adminCount == 1 {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "cannot remove last admin"})
				return
			}
		}

		// Update the role
		_, err = tx.Exec(r.Context(), `
			UPDATE organization_members
			SET role = $1
			WHERE org_id = $2 AND user_id = $3
		`, req.Role, sess.OrgID, userID)
		if err != nil {
			log.WithError(err).Error("change role: update")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if err = tx.Commit(r.Context()); err != nil {
			log.WithError(err).Error("change role: commit")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Return updated member
		var name, email string
		var joinedAt time.Time
		err = db.QueryRow(r.Context(), `
			SELECT u.name, u.email, om.joined_at
			FROM organization_members om
			JOIN users u ON om.user_id = u.id
			WHERE om.org_id = $1 AND om.user_id = $2
		`, sess.OrgID, userID).Scan(&name, &email, &joinedAt)
		if err != nil {
			log.WithError(err).Error("change role: fetch updated member")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		m := MemberItem{ID: userID, Name: name, Email: email, Role: req.Role, JoinedAt: joinedAt}
		writeJSON(w, http.StatusOK, map[string]any{"data": m})
	}
}

// HandleRemoveMember returns a handler for DELETE /v1/team/members/:userId.
// Removes a member from the organization. Admin-only. Cannot demote the last admin.
// @Summary Remove Member
// @Description Remove Member endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/team/members/{userId} [delete]
func HandleRemoveMember(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if !requireAdmin(w, sess) {
			return
		}

		userID, err := strconv.ParseInt(chi.URLParam(r, "userId"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid userId"})
			return
		}

		tx, err := db.Begin(r.Context())
		if err != nil {
			log.WithError(err).Error("remove member: begin tx")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer tx.Rollback(r.Context()) //nolint:errcheck

		// Check if member exists and get their role
		var role string
		err = tx.QueryRow(r.Context(), `
			SELECT role FROM organization_members
			WHERE org_id = $1 AND user_id = $2
		`, sess.OrgID, userID).Scan(&role)
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "member not found"})
			return
		}
		if err != nil {
			log.WithError(err).Error("remove member: query role")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// If removing an admin, check if this is the last admin
		if role == "admin" {
			var adminCount int64
			err = tx.QueryRow(r.Context(), `
				SELECT COUNT(*) FROM organization_members
				WHERE org_id = $1 AND role = 'admin'
			`, sess.OrgID).Scan(&adminCount)
			if err != nil {
				log.WithError(err).Error("remove member: count admins")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			if adminCount == 1 {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "cannot remove last admin"})
				return
			}
		}

		// Remove the member
		_, err = tx.Exec(r.Context(), `
			DELETE FROM organization_members
			WHERE org_id = $1 AND user_id = $2
		`, sess.OrgID, userID)
		if err != nil {
			log.WithError(err).Error("remove member: delete")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if err = tx.Commit(r.Context()); err != nil {
			log.WithError(err).Error("remove member: commit")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"message": "member removed"})
	}
}

// HandleCreateInvitation returns a handler for POST /v1/team/invitations.
// Creates a new invitation with a 32-byte token and 7-day TTL. Admin-only.
// @Summary Create Invitation
// @Description Create Invitation endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/team/invitations [post]
func HandleCreateInvitation(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if !requireAdmin(w, sess) {
			return
		}

		var req CreateInvitationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}

		// Validate email
		if req.Email == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email is required"})
			return
		}
		if _, err := mail.ParseAddress(req.Email); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid email address"})
			return
		}

		// Validate role
		if req.Role != "admin" && req.Role != "member" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be 'admin' or 'member'"})
			return
		}

		// Normalize email to lowercase for consistency
		req.Email = strings.ToLower(req.Email)

		// Check if email already a member of org
		var exists bool
		err := db.QueryRow(r.Context(), `
			SELECT EXISTS(
				SELECT 1 FROM organization_members om
				JOIN users u ON om.user_id = u.id
				WHERE om.org_id = $1 AND LOWER(u.email) = LOWER($2)
			)
		`, sess.OrgID, req.Email).Scan(&exists)
		if err != nil {
			log.WithError(err).Error("create invitation: check existing member")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if exists {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "email already a member"})
			return
		}

		// Generate 32-byte random token (64 hex chars)
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			log.WithError(err).Error("create invitation: generate token")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		token := hex.EncodeToString(tokenBytes)

		// Insert invitation with 7-day TTL
		expiresAt := time.Now().UTC().Add(7 * 24 * time.Hour)
		_, err = db.Exec(r.Context(), `
			INSERT INTO invitations (token, org_id, email, role, invited_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, token, sess.OrgID, req.Email, req.Role, sess.UserID, expiresAt)
		if err != nil {
			log.WithError(err).Error("create invitation: insert")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		inv := InvitationItem{
			ID:        token,
			Email:     req.Email,
			Role:      req.Role,
			ExpiresAt: expiresAt,
		}
		writeJSON(w, http.StatusCreated, map[string]any{"data": inv})
	}
}

// HandleListInvitations returns a handler for GET /v1/team/invitations.
// Lists pending (non-accepted, non-expired) invitations. Admin-only.
func HandleListInvitations(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if !requireAdmin(w, sess) {
			return
		}

		rows, err := db.Query(r.Context(), `
			SELECT token, email, role, expires_at
			FROM invitations
			WHERE org_id = $1 AND accepted_at IS NULL AND expires_at > now()
			ORDER BY expires_at ASC
		`, sess.OrgID)
		if err != nil {
			log.WithError(err).Error("list invitations: query")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer rows.Close()

		var invitations []InvitationItem
		for rows.Next() {
			var inv InvitationItem
			if err := rows.Scan(&inv.ID, &inv.Email, &inv.Role, &inv.ExpiresAt); err != nil {
				log.WithError(err).Error("list invitations: scan")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			invitations = append(invitations, inv)
		}
		if invitations == nil {
			invitations = []InvitationItem{} // return empty array, not null
		}

		writeJSON(w, http.StatusOK, map[string]any{"data": invitations})
	}
}

// HandleRevokeInvitation returns a handler for DELETE /v1/team/invitations/:token.
// Revokes a pending invitation. Admin-only.
// @Summary Revoke Invitation
// @Description Revoke Invitation endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/team/invitations/{token} [delete]
func HandleRevokeInvitation(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if !requireAdmin(w, sess) {
			return
		}

		token := chi.URLParam(r, "token")
		if token == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token is required"})
			return
		}

		result, err := db.Exec(r.Context(), `
			DELETE FROM invitations
			WHERE token = $1 AND org_id = $2
		`, token, sess.OrgID)
		if err != nil {
			log.WithError(err).Error("revoke invitation: delete")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if result.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "invitation not found"})
			return
		}

		writeJSON(w, http.StatusOK, map[string]string{"message": "invitation revoked"})
	}
}

// HandleAcceptInvitation returns a handler for POST /v1/team/invitations/:token/accept.
// Accepts an invitation, creates the user, and adds them to the organization.
// No authentication required (first-time user signup via invitation).
// @Summary Accept Invitation
// @Description Accept Invitation endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/team/invitations/{token}/accept [post]
func HandleAcceptInvitation(
	db *pgxpool.Pool,
	sessions *session.Store,
	log *logrus.Logger,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		if token == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token is required"})
			return
		}

		var req AcceptInvitationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}

		if req.Name == "" || req.Password == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and password are required"})
			return
		}

		// Validate password strength (minimum 8 characters)
		if len(req.Password) < 8 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 8 characters"})
			return
		}

		tx, err := db.Begin(r.Context())
		if err != nil {
			log.WithError(err).Error("accept invitation: begin tx")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer tx.Rollback(r.Context()) //nolint:errcheck

		// Fetch invitation
		var email, role string
		var expiresAt time.Time
		var acceptedAt *time.Time
		var orgID int64
		err = tx.QueryRow(r.Context(), `
			SELECT email, role, expires_at, accepted_at, org_id
			FROM invitations
			WHERE token = $1
		`, token).Scan(&email, &role, &expiresAt, &acceptedAt, &orgID)
		if err == pgx.ErrNoRows {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "invitation not found"})
			return
		}
		if err != nil {
			log.WithError(err).Error("accept invitation: query")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Check if expired
		if expiresAt.Before(time.Now().UTC()) {
			writeJSON(w, http.StatusGone, map[string]string{"error": "invitation expired"})
			return
		}

		// Check if already accepted
		if acceptedAt != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invitation already accepted"})
			return
		}

		// Hash password
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
		if err != nil {
			log.WithError(err).Error("accept invitation: bcrypt")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Create user (if not already exists)
		var userID int64
		err = tx.QueryRow(r.Context(), `
			INSERT INTO users (email, name, password_hash)
			VALUES ($1, $2, $3)
			ON CONFLICT (email) DO UPDATE SET name = $2, password_hash = $3
			RETURNING id
		`, email, req.Name, string(hash)).Scan(&userID)
		if err != nil {
			log.WithError(err).Error("accept invitation: insert user")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Add user to organization
		_, err = tx.Exec(r.Context(), `
			INSERT INTO organization_members (org_id, user_id, role, invited_by, joined_at)
			VALUES ($1, $2, $3, (SELECT invited_by FROM invitations WHERE token = $4), now())
			ON CONFLICT (org_id, user_id) DO NOTHING
		`, orgID, userID, role, token)
		if err != nil {
			log.WithError(err).Error("accept invitation: insert org member")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Mark invitation as accepted
		_, err = tx.Exec(r.Context(), `
			UPDATE invitations
			SET accepted_at = now()
			WHERE token = $1
		`, token)
		if err != nil {
			log.WithError(err).Error("accept invitation: mark accepted")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if err = tx.Commit(r.Context()); err != nil {
			log.WithError(err).Error("accept invitation: commit")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Create session
		sessionToken, err := sessions.Create(r.Context(), session.Data{
			UserID: userID,
			OrgID:  orgID,
			Role:   role,
			Email:  email,
			Name:   req.Name,
		})
		if err != nil {
			log.WithError(err).Error("accept invitation: create session")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		// Set session cookie
		http.SetCookie(w, &http.Cookie{
			Name:     "session",
			Value:    sessionToken,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   8 * 3600, // 8 hours
		})

		m := MemberItem{
			ID:    userID,
			Name:  req.Name,
			Email: email,
			Role:  role,
		}
		writeJSON(w, http.StatusCreated, map[string]any{"data": m})
	}
}
