package emos

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// RunJob computes eMOS for every session in conferenceID that has at least one
// connection_stats row and writes the result to session_quality. Sessions with
// no stats are silently skipped. The upsert makes the job idempotent.
//
// lossCoeff is the high-loss impairment coefficient K from RFC §3.7; pass
// DefaultLossCoeff unless per-deployment tuning is required.
func RunJob(ctx context.Context, db *pgxpool.Pool, conferenceID int64, lossCoeff float64, log *logrus.Logger) error {
	// percentile_cont aggregates median loss and RTT across all connections in a
	// session. Sessions with no matching connection_stats rows produce no output
	// rows (implicit HAVING COUNT > 0 from the inner JOIN).
	rows, err := db.Query(ctx, `
		SELECT
		    s.id,
		    percentile_cont(0.5) WITHIN GROUP (ORDER BY cs.packets_lost_rate),
		    percentile_cont(0.5) WITHIN GROUP (ORDER BY cs.rtt_ms)
		FROM sessions s
		JOIN participants p    ON p.id           = s.participant_id
		JOIN connections conn  ON conn.session_id = s.id
		JOIN connection_stats cs ON cs.connection_id = conn.id
		WHERE p.conference_id = $1
		  AND cs.packets_lost_rate IS NOT NULL
		  AND cs.rtt_ms IS NOT NULL
		GROUP BY s.id
	`, conferenceID)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			sessionID  int64
			medianLoss float64
			medianRTT  float64
		)
		if err := rows.Scan(&sessionID, &medianLoss, &medianRTT); err != nil {
			return err
		}

		emos, outcome := ComputeEMOS(medianLoss, medianRTT, lossCoeff)

		if _, err := db.Exec(ctx, `
			INSERT INTO session_quality (session_id, emos, outcome, computed_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (session_id) DO UPDATE
			SET emos = EXCLUDED.emos, outcome = EXCLUDED.outcome, computed_at = now()
		`, sessionID, emos, outcome); err != nil {
			if log != nil {
				log.WithError(err).WithField("session_id", sessionID).Error("emos job: upsert session_quality")
			}
			return err
		}

		if log != nil {
			log.WithFields(logrus.Fields{
				"conference_id": conferenceID,
				"session_id":    sessionID,
				"emos":          emos,
				"outcome":       outcome,
			}).Info("emos job: session_quality written")
		}
	}
	return rows.Err()
}
