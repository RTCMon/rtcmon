// Package ingest implements the FlushFn used by the worker pool to persist
// batches of ingest payloads to PostgreSQL. Each flush upserts the entity
// hierarchy (conferences → participants → sessions → connections) and then
// bulk-inserts connection_stats rows via pgx.CopyFrom.
//
// EWMA state is maintained per connection across flush calls (α = 0.2).
// On CopyFrom failure a dead-letter JSON file is written so the batch can be
// replayed manually.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/model"
)

const ewmaAlpha = 0.2

// ewmaState holds the running EWMA values for one connection.
type ewmaState struct {
	PacketLossRate float64
	JitterMs       float64
	RTTMs          float64
	BitrateIn      float64
	BitrateOut     float64
}

func (s *ewmaState) update(raw model.StatSnapshot) {
	s.PacketLossRate = ewmaAlpha*raw.PacketLossRate + (1-ewmaAlpha)*s.PacketLossRate
	s.JitterMs = ewmaAlpha*raw.JitterMs + (1-ewmaAlpha)*s.JitterMs
	s.RTTMs = ewmaAlpha*raw.RTTMs + (1-ewmaAlpha)*s.RTTMs
	s.BitrateIn = ewmaAlpha*raw.BitrateInKbps + (1-ewmaAlpha)*s.BitrateIn
	s.BitrateOut = ewmaAlpha*raw.BitrateOutKbps + (1-ewmaAlpha)*s.BitrateOut
}

func newEWMA(raw model.StatSnapshot) *ewmaState {
	return &ewmaState{
		PacketLossRate: raw.PacketLossRate,
		JitterMs:       raw.JitterMs,
		RTTMs:          raw.RTTMs,
		BitrateIn:      raw.BitrateInKbps,
		BitrateOut:     raw.BitrateOutKbps,
	}
}

// Flusher persists batches of IngestPayload to Postgres. Create with New.
// Safe for concurrent use by multiple worker goroutines.
type Flusher struct {
	pool          *pgxpool.Pool
	log           *logrus.Logger
	deadLetterDir string

	ewmaMu    sync.Mutex
	ewmaState map[string]*ewmaState // keyed by connection external_id
}

// New creates a Flusher. deadLetterDir is created on first use if absent.
func New(pool *pgxpool.Pool, log *logrus.Logger, deadLetterDir string) *Flusher {
	return &Flusher{
		pool:          pool,
		log:           log,
		deadLetterDir: deadLetterDir,
		ewmaState:     make(map[string]*ewmaState),
	}
}

// Flush implements worker.FlushFn. It upserts the entity hierarchy, computes
// EWMA values, and bulk-inserts connection_stats rows. On CopyFrom failure it
// writes a dead-letter file and logs the error.
func (f *Flusher) Flush(ctx context.Context, batch []model.IngestPayload) {
	if len(batch) == 0 {
		return
	}

	if err := f.flush(ctx, batch); err != nil {
		f.log.WithError(err).Error("ingest: flush failed, writing dead-letter")
		f.writeDeadLetter(batch)
	}
}

func (f *Flusher) flush(ctx context.Context, batch []model.IngestPayload) error {
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// 1. Upsert conferences → map[conferenceExtID]dbID
	confIDs, err := f.upsertConferences(ctx, tx, batch)
	if err != nil {
		return fmt.Errorf("upsert conferences: %w", err)
	}

	// 2. Upsert participants → map[confDBID+"::"+userID]dbID
	partIDs, err := f.upsertParticipants(ctx, tx, batch, confIDs)
	if err != nil {
		return fmt.Errorf("upsert participants: %w", err)
	}

	// 3. Upsert sessions → map[sessionExtID]dbID
	sessIDs, err := f.upsertSessions(ctx, tx, batch, partIDs, confIDs)
	if err != nil {
		return fmt.Errorf("upsert sessions: %w", err)
	}

	// 4. Upsert connections → map[connectionExtID]dbID
	connIDs, err := f.upsertConnections(ctx, tx, batch, sessIDs)
	if err != nil {
		return fmt.Errorf("upsert connections: %w", err)
	}

	// 5. Compute EWMA and build CopyFrom rows
	rows := f.buildStatRows(batch, connIDs)

	// 6. Bulk insert connection_stats
	_, err = tx.CopyFrom(
		ctx,
		pgx.Identifier{"connection_stats"},
		[]string{
			"connection_id", "ts",
			"packets_lost_rate", "jitter_ms", "rtt_ms",
			"bitrate_in_kbps", "bitrate_out_kbps",
			"ewma_packets_lost_rate", "ewma_jitter_ms", "ewma_rtt_ms",
			"ewma_bitrate_in_kbps", "ewma_bitrate_out_kbps",
			"fps", "frame_width", "frame_height",
			"audio_level", "concealment_ratio",
		},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("copy from: %w", err)
	}

	return tx.Commit(ctx)
}

// upsertConferences returns a map from conference external_id to its DB id.
// Unique key: (app_id, external_id).
func (f *Flusher) upsertConferences(
	ctx context.Context, tx pgx.Tx, batch []model.IngestPayload,
) (map[string]int64, error) {
	type key struct{ appID int64; extID string }
	seen := make(map[key]bool)
	result := make(map[string]int64)

	for _, p := range batch {
		appID, err := strconv.ParseInt(p.AppID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid app_id %q: %w", p.AppID, err)
		}
		k := key{appID, p.ConferenceID}
		if seen[k] {
			continue
		}
		seen[k] = true

		var dbID int64
		err = tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO conferences (app_id, external_id)
				VALUES ($1, $2)
				ON CONFLICT (app_id, external_id) DO NOTHING
				RETURNING id
			)
			SELECT id FROM ins
			UNION ALL
			SELECT id FROM conferences WHERE app_id = $1 AND external_id = $2
			LIMIT 1
		`, appID, p.ConferenceID).Scan(&dbID)
		if err != nil {
			return nil, fmt.Errorf("conference %q: %w", p.ConferenceID, err)
		}
		result[p.ConferenceID] = dbID
	}
	return result, nil
}

// upsertParticipants returns a map from "confDBID::userID" to participant DB id.
func (f *Flusher) upsertParticipants(
	ctx context.Context, tx pgx.Tx, batch []model.IngestPayload,
	confIDs map[string]int64,
) (map[string]int64, error) {
	type key struct {
		confDBID int64
		userID   string
	}
	seen := make(map[key]bool)
	result := make(map[string]int64)

	for _, p := range batch {
		confDBID := confIDs[p.ConferenceID]
		k := key{confDBID, p.UserID}
		if seen[k] {
			continue
		}
		seen[k] = true

		var dbID int64
		err := tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO participants (conference_id, user_id)
				VALUES ($1, $2)
				ON CONFLICT (conference_id, user_id) DO NOTHING
				RETURNING id
			)
			SELECT id FROM ins
			UNION ALL
			SELECT id FROM participants WHERE conference_id = $1 AND user_id = $2
			LIMIT 1
		`, confDBID, p.UserID).Scan(&dbID)
		if err != nil {
			return nil, fmt.Errorf("participant conf=%d user=%q: %w", confDBID, p.UserID, err)
		}
		mapKey := participantKey(confDBID, p.UserID)
		result[mapKey] = dbID
	}
	return result, nil
}

// upsertSessions returns a map from session external_id to DB id.
func (f *Flusher) upsertSessions(
	ctx context.Context, tx pgx.Tx, batch []model.IngestPayload,
	partIDs map[string]int64, confIDs map[string]int64,
) (map[string]int64, error) {
	seen := make(map[string]bool)
	result := make(map[string]int64)

	for _, p := range batch {
		if seen[p.SessionID] {
			continue
		}
		seen[p.SessionID] = true

		confDBID := confIDs[p.ConferenceID]
		partDBID := partIDs[participantKey(confDBID, p.UserID)]

		var dbID int64
		err := tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO sessions (participant_id, external_id)
				VALUES ($1, $2)
				ON CONFLICT (external_id) DO NOTHING
				RETURNING id
			)
			SELECT id FROM ins
			UNION ALL
			SELECT id FROM sessions WHERE external_id = $2
			LIMIT 1
		`, partDBID, p.SessionID).Scan(&dbID)
		if err != nil {
			return nil, fmt.Errorf("session %q: %w", p.SessionID, err)
		}
		result[p.SessionID] = dbID
	}
	return result, nil
}

// upsertConnections returns a map from connection external_id to DB id.
// started_at is set to the minimum ts in the batch for that connection.
func (f *Flusher) upsertConnections(
	ctx context.Context, tx pgx.Tx, batch []model.IngestPayload,
	sessIDs map[string]int64,
) (map[string]int64, error) {
	// Pre-compute min ts per connection for started_at.
	minTS := make(map[string]int64)
	for _, p := range batch {
		for _, ev := range p.Events {
			if cur, ok := minTS[p.ConnectionID]; !ok || ev.TS < cur {
				minTS[p.ConnectionID] = ev.TS
			}
		}
	}

	seen := make(map[string]bool)
	result := make(map[string]int64)

	for _, p := range batch {
		if seen[p.ConnectionID] {
			continue
		}
		seen[p.ConnectionID] = true

		sessDBID := sessIDs[p.SessionID]
		startedAt := time.Unix(0, minTS[p.ConnectionID]*int64(time.Millisecond)).UTC()

		var dbID int64
		err := tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO connections (session_id, external_id, started_at)
				VALUES ($1, $2, $3)
				ON CONFLICT (external_id) DO NOTHING
				RETURNING id
			)
			SELECT id FROM ins
			UNION ALL
			SELECT id FROM connections WHERE external_id = $2
			LIMIT 1
		`, sessDBID, p.ConnectionID, startedAt).Scan(&dbID)
		if err != nil {
			return nil, fmt.Errorf("connection %q: %w", p.ConnectionID, err)
		}
		result[p.ConnectionID] = dbID
	}
	return result, nil
}

// buildStatRows computes EWMA values for each stat snapshot and returns the
// rows for pgx.CopyFromRows. The EWMA state map is updated under a mutex.
func (f *Flusher) buildStatRows(
	batch []model.IngestPayload,
	connIDs map[string]int64,
) [][]any {
	f.ewmaMu.Lock()
	defer f.ewmaMu.Unlock()

	var rows [][]any
	for _, p := range batch {
		connDBID := connIDs[p.ConnectionID]
		for _, ev := range p.Events {
			state, exists := f.ewmaState[p.ConnectionID]
			if !exists {
				state = newEWMA(ev)
				f.ewmaState[p.ConnectionID] = state
			} else {
				state.update(ev)
			}

			ts := time.Unix(0, ev.TS*int64(time.Millisecond)).UTC()

			rows = append(rows, []any{
				connDBID, ts,
				float32(ev.PacketLossRate), float32(ev.JitterMs), float32(ev.RTTMs),
				int32(ev.BitrateInKbps), int32(ev.BitrateOutKbps), //nolint:gosec
				float32(state.PacketLossRate), float32(state.JitterMs), float32(state.RTTMs),
				float32(state.BitrateIn), float32(state.BitrateOut),
				int32(ev.FPS), int32(ev.FrameWidth), int32(ev.FrameHeight), //nolint:gosec
				float32(ev.AudioLevel), float32(ev.ConcealmentRatio),
			})
		}
	}
	return rows
}

// writeDeadLetter marshals the batch to JSON and writes it to a file in
// deadLetterDir. Errors here are logged but do not propagate — the original
// flush error is the primary concern.
func (f *Flusher) writeDeadLetter(batch []model.IngestPayload) {
	if err := os.MkdirAll(f.deadLetterDir, 0o755); err != nil {
		f.log.WithError(err).Error("ingest: dead-letter: cannot create dir")
		return
	}

	data, err := json.Marshal(batch)
	if err != nil {
		f.log.WithError(err).Error("ingest: dead-letter: cannot marshal batch")
		return
	}

	name := fmt.Sprintf("dead_%d.json", time.Now().UnixNano())
	path := filepath.Join(f.deadLetterDir, name)

	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec
		f.log.WithError(err).WithField("path", path).Error("ingest: dead-letter: write failed")
		return
	}

	f.log.WithField("path", path).Warn("ingest: dead-letter file written")
}

func participantKey(confDBID int64, userID string) string {
	return strconv.FormatInt(confDBID, 10) + "::" + userID
}
