package emos

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RTCMon/rtcmon/internal/metrics"
)

// StatRow represents one connection_stats sample (ordered by ts).
type StatRow struct {
	Timestamp             time.Time
	PacketLossRatio       float64 // [0, 1]
	RTT                   int32   // milliseconds
	Jitter                int32   // milliseconds
	VideoFreezeCount      int     // incremented when freeze occurs
	AudioConcealmentRatio float64 // [0, 1]
	OutboundBitrate       int64   // bps
}

// Observation represents a detected issue.
type Observation struct {
	RuleName string
	Severity string // "info", "warning", "critical"
	Message  string
	StartTS  time.Time
	EndTS    time.Time
	Context  map[string]interface{}
}

// ObservationRule defines a detection rule.
type ObservationRule struct {
	Name     string
	Severity string
	Evaluate func(samples []StatRow, thresholds map[string]interface{}) []Observation
}

// ObservationEngine coordinates rule evaluation.
type ObservationEngine struct {
	rules    []ObservationRule
	db       *pgxpool.Pool
	defaults map[string]interface{} // default thresholds
}

// NewObservationEngine creates a new observations engine with default rules.
func NewObservationEngine(db *pgxpool.Pool) *ObservationEngine {
	return &ObservationEngine{
		rules: DefaultRules(),
		db:    db,
		defaults: map[string]interface{}{
			"packet_loss_sustained.threshold":    0.05,
			"packet_loss_spike.threshold":        0.15,
			"rtt_high.threshold":                 float64(300),
			"rtt_spike_delta.threshold":          float64(100),
			"jitter_high.threshold":              float64(50),
			"audio_concealment_high.threshold":   0.10,
			"bitrate_collapse_ratio.threshold":   0.50,
			"packet_loss_sustained.min_samples":  float64(3),
			"rtt_high.min_samples":               float64(2),
			"jitter_high.min_samples":            float64(3),
			"audio_concealment_high.min_samples": float64(3),
		},
	}
}

// EvaluateConnection analyzes a single connection and writes observations to events.
func (e *ObservationEngine) EvaluateConnection(
	ctx context.Context,
	connectionID int64,
	appID int64,
) error {
	// 1. Load stats for connection.
	stats, err := e.loadConnectionStats(ctx, connectionID)
	if err != nil {
		return fmt.Errorf("emos: load stats: %w", err)
	}
	if len(stats) == 0 {
		return nil // No stats, no observations.
	}

	// 2. Load app config.
	thresholds, err := e.loadThresholds(ctx, appID)
	if err != nil {
		return fmt.Errorf("emos: load thresholds: %w", err)
	}

	// 3. Evaluate all rules.
	var allObservations []Observation
	for _, rule := range e.rules {
		if rule.Name == "ice_failure" {
			// Special handling: fetch from events.
			obs, err := e.evaluateICEFailure(ctx, connectionID, thresholds)
			if err != nil {
				// Log but continue.
				continue
			}
			allObservations = append(allObservations, obs...)
		} else {
			obs := rule.Evaluate(stats, thresholds)
			allObservations = append(allObservations, obs...)
		}
	}

	// 4. Deduplicate and insert into events table.
	for _, obs := range allObservations {
		if err := e.insertObservationIfNew(ctx, connectionID, obs); err != nil {
			// Log but continue on error (non-blocking).
			continue
		}
	}

	return nil
}

// loadConnectionStats queries connection_stats ordered by ts.
func (e *ObservationEngine) loadConnectionStats(ctx context.Context, connectionID int64) ([]StatRow, error) {
	rows, err := e.db.Query(ctx, `
		SELECT 
			ts,
			packets_lost_rate,
			rtt_ms,
			jitter_ms,
			COALESCE(audio_level, 0),
			concealment_ratio,
			bitrate_out_kbps
		FROM connection_stats
		WHERE connection_id = $1
		ORDER BY ts ASC
	`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("emos: query stats: %w", err)
	}
	defer rows.Close()

	var stats []StatRow
	for rows.Next() {
		var s StatRow
		var audioLevel, concealmentRatio float32
		var bitrateOutKbps int32
		if err := rows.Scan(
			&s.Timestamp,
			&s.PacketLossRatio,
			&s.RTT,
			&s.Jitter,
			&audioLevel,
			&concealmentRatio,
			&bitrateOutKbps,
		); err != nil {
			return nil, fmt.Errorf("emos: scan row: %w", err)
		}
		s.AudioConcealmentRatio = float64(concealmentRatio)
		s.OutboundBitrate = int64(bitrateOutKbps) * 1000 // Convert kbps to bps.
		stats = append(stats, s)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("emos: iterate rows: %w", err)
	}

	return stats, nil
}

// loadThresholds merges app config with defaults.
func (e *ObservationEngine) loadThresholds(ctx context.Context, appID int64) (map[string]interface{}, error) {
	var configJSON []byte
	err := e.db.QueryRow(ctx, `
		SELECT COALESCE(observation_config, '{}'::jsonb)
		FROM apps
		WHERE id = $1
	`, appID).Scan(&configJSON)
	if err != nil {
		return nil, fmt.Errorf("emos: query app config: %w", err)
	}

	// Start with defaults.
	thresholds := make(map[string]interface{})
	for k, v := range e.defaults {
		thresholds[k] = v
	}

	// Merge app config (may override defaults).
	var appConfig map[string]interface{}
	if err := json.Unmarshal(configJSON, &appConfig); err != nil {
		// Log and use defaults.
		return thresholds, nil
	}

	// Merge app config into thresholds.
	for k, v := range appConfig {
		thresholds[k] = v
	}

	return thresholds, nil
}

// evaluateICEFailure checks for ICE failures in the events table.
func (e *ObservationEngine) evaluateICEFailure(ctx context.Context, connectionID int64, thresholds map[string]interface{}) ([]Observation, error) {
	rows, err := e.db.Query(ctx, `
		SELECT ts
		FROM events
		WHERE connection_id = $1
			AND event_type = 'ice_connection_state_change'
			AND payload->>'state' = 'failed'
		ORDER BY ts ASC
	`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("emos: query ice failures: %w", err)
	}
	defer rows.Close()

	var observations []Observation
	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			return nil, fmt.Errorf("emos: scan ice failure: %w", err)
		}
		observations = append(observations, Observation{
			RuleName: "ice_failure",
			Severity: "critical",
			Message:  "ICE connection failed",
			StartTS:  ts,
			EndTS:    ts,
			Context: map[string]interface{}{
				"event_type": "ice_connection_state_change",
			},
		})
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("emos: iterate ice failures: %w", err)
	}

	return observations, nil
}

// insertObservationIfNew checks for duplicates before inserting into events table.
func (e *ObservationEngine) insertObservationIfNew(ctx context.Context, connectionID int64, obs Observation) error {
	// Check for duplicate observation.
	var exists bool
	err := e.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM events
			WHERE connection_id = $1
				AND event_type = 'observation'
				AND payload->>'rule_name' = $2
				AND payload->>'start_ts' = $3
				AND payload->>'end_ts' = $4
		)
	`, connectionID, obs.RuleName, obs.StartTS.Format(time.RFC3339), obs.EndTS.Format(time.RFC3339)).Scan(&exists)
	if err != nil {
		return fmt.Errorf("emos: check duplicate: %w", err)
	}
	if exists {
		return nil // Already exists, skip.
	}

	// Build payload.
	payload := map[string]interface{}{
		"rule_name": obs.RuleName,
		"severity":  obs.Severity,
		"message":   obs.Message,
		"start_ts":  obs.StartTS.Format(time.RFC3339),
		"end_ts":    obs.EndTS.Format(time.RFC3339),
		"context":   obs.Context,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("emos: marshal payload: %w", err)
	}

	// Insert into events table.
	_, err = e.db.Exec(ctx, `
		INSERT INTO events (connection_id, session_id, event_type, payload, ts)
		SELECT $1, connections.session_id, 'observation'::text, $2::jsonb, $3
		FROM connections
		WHERE connections.id = $1
	`, connectionID, payloadJSON, obs.StartTS)
	if err != nil {
		return fmt.Errorf("emos: insert observation: %w", err)
	}

	metrics.ObservationsTriggeredTotal.WithLabelValues(obs.RuleName).Inc()
	return nil
}
