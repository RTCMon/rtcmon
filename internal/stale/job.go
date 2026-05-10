// Package stale implements the stale-conference cleanup background job.
// It periodically finds conferences that have no ended_at and no recent
// connection_stats, closes them, and fires the eMOS computation.
package stale

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// Job runs the stale-conference cleanup on a configurable ticker interval.
// Create with New, then call Start once. Stop cleanly with Shutdown.
type Job struct {
	db            *pgxpool.Pool
	log           *logrus.Logger
	checkInterval time.Duration
	idleThreshold time.Duration
	lossCoeff     float64
	triggerEMOS   func(int64)

	once    sync.Once
	done    chan struct{}
	stopped chan struct{}
}

// New creates a Job. Start must be called separately.
func New(
	db *pgxpool.Pool,
	log *logrus.Logger,
	checkInterval time.Duration,
	idleThreshold time.Duration,
	lossCoeff float64,
	triggerEMOS func(int64),
) *Job {
	return &Job{
		db:            db,
		log:           log,
		checkInterval: checkInterval,
		idleThreshold: idleThreshold,
		lossCoeff:     lossCoeff,
		triggerEMOS:   triggerEMOS,
		done:          make(chan struct{}),
		stopped:       make(chan struct{}),
	}
}

// Start launches the background goroutine. Call only once.
func (j *Job) Start() {
	go func() {
		defer close(j.stopped)

		ticker := time.NewTicker(j.checkInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := RunOnce(context.Background(), j.db, j.idleThreshold, j.lossCoeff, j.triggerEMOS, j.log); err != nil {
					j.log.WithError(err).Error("stale job: scan failed")
				}
			case <-j.done:
				return
			}
		}
	}()
}

// Shutdown signals the goroutine to stop and waits for it to exit.
// Safe to call multiple times.
func (j *Job) Shutdown() {
	j.once.Do(func() { close(j.done) })
	<-j.stopped
}

// RunOnce performs a single scan: finds stale conferences, sets ended_at, and
// fires the eMOS trigger for each. Exported so tests can invoke it directly.
func RunOnce(
	ctx context.Context,
	db *pgxpool.Pool,
	idleThreshold time.Duration,
	lossCoeff float64,
	triggerEMOS func(int64),
	log *logrus.Logger,
) error {
	// Find all conferences that have no ended_at and no connection_stats row
	// newer than idleThreshold. Conferences that never sent any stats are
	// also returned (NOT EXISTS is vacuously true with no rows).
	// Pass idleThreshold as time.Duration; pgx v5 encodes it as a Postgres
	// interval natively, so no string formatting or ::interval cast is needed.
	rows, err := db.Query(ctx, `
		SELECT c.id
		FROM conferences c
		WHERE c.ended_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1
		      FROM participants p
		      JOIN sessions s       ON s.participant_id  = p.id
		      JOIN connections conn ON conn.session_id   = s.id
		      JOIN connection_stats cs ON cs.connection_id = conn.id
		      WHERE p.conference_id = c.id
		        AND cs.ts > now() - $1
		  )
	`, idleThreshold)
	if err != nil {
		return err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, confID := range ids {
		// Idempotent update: AND ended_at IS NULL ensures two concurrent runs
		// cannot both succeed for the same conference.
		tag, err := db.Exec(ctx,
			`UPDATE conferences SET ended_at = now() WHERE id = $1 AND ended_at IS NULL`,
			confID,
		)
		if err != nil {
			if log != nil {
				log.WithError(err).WithField("conference_id", confID).Error("stale job: update ended_at")
			}
			return err
		}

		// Only trigger eMOS when this run actually closed the conference.
		if tag.RowsAffected() == 0 {
			continue
		}

		if log != nil {
			log.WithField("conference_id", confID).Info("stale job: conference closed")
		}

		if triggerEMOS != nil {
			triggerEMOS(confID)
		}
	}

	return nil
}
