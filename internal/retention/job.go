// Package retention implements the nightly data-retention cleanup job.
// It drops old connection_stats partitions and batch-deletes old events rows
// according to each app's retention_days setting.
package retention

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

const batchSize = 10_000

// Job runs the data-retention cleanup on a nightly cron schedule.
// Create with New, then call Start once. Stop cleanly with Shutdown.
type Job struct {
	db      *pgxpool.Pool
	log     *logrus.Logger
	cronStr string

	once    sync.Once
	done    chan struct{}
	stopped chan struct{}
	cancel  context.CancelFunc
}

// New creates a Job. Start must be called separately.
func New(db *pgxpool.Pool, log *logrus.Logger, cronStr string) *Job {
	return &Job{
		db:      db,
		log:     log,
		cronStr: cronStr,
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
		cancel:  func() {},
	}
}

// Start launches the background goroutine. Call only once.
func (j *Job) Start() {
	hour, minute, err := parseDailyCron(j.cronStr)
	if err != nil {
		j.log.WithError(err).WithField("cron", j.cronStr).Error("retention: invalid cron expression, job not started")
		close(j.stopped)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel

	go func() {
		defer close(j.stopped)
		for {
			next := nextRunTime(hour, minute, time.Now().UTC())
			timer := time.NewTimer(time.Until(next))
			select {
			case <-timer.C:
				if err := RunOnce(ctx, j.db, j.log); err != nil {
					j.log.WithError(err).Error("retention: job run failed")
				}
			case <-j.done:
				timer.Stop()
				return
			}
		}
	}()
}

// Shutdown signals the goroutine to stop and waits for it to exit.
// Safe to call multiple times.
func (j *Job) Shutdown() {
	j.once.Do(func() {
		j.cancel()
		close(j.done)
	})
	<-j.stopped
}

// RunOnce performs a single retention pass: drops old connection_stats partitions
// and batch-deletes old events rows. Exported so tests can invoke it directly.
func RunOnce(ctx context.Context, db *pgxpool.Pool, log *logrus.Logger) error {
	type appRow struct {
		id            int64
		retentionDays int
	}

	rows, err := db.Query(ctx, `SELECT id, retention_days FROM apps WHERE retention_days > 0`)
	if err != nil {
		return fmt.Errorf("retention: query apps: %w", err)
	}
	defer rows.Close()

	var apps []appRow
	for rows.Next() {
		var a appRow
		if err := rows.Scan(&a.id, &a.retentionDays); err != nil {
			return fmt.Errorf("retention: scan app: %w", err)
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("retention: iterate apps: %w", err)
	}

	if len(apps) == 0 {
		return nil
	}

	// ─── Step 1: drop old connection_stats partitions ──────────────────────────
	// Partitions contain data for all apps, so use the minimum retention_days
	// across all apps as the drop boundary (conservative, no data lost).
	minRetentionDays := apps[0].retentionDays
	for _, a := range apps[1:] {
		if a.retentionDays < minRetentionDays {
			minRetentionDays = a.retentionDays
		}
	}

	partitionsDropped, err := dropOldPartitions(ctx, db, minRetentionDays, log)
	if err != nil {
		return err
	}
	if log != nil {
		log.WithField("partitions_dropped", partitionsDropped).
			WithField("min_retention_days", minRetentionDays).
			Info("retention: partition cleanup complete")
	}

	// ─── Step 2: batch-delete old events rows per app ──────────────────────────
	for _, a := range apps {
		cutoff := time.Now().UTC().AddDate(0, 0, -a.retentionDays)
		totalDeleted, batches, err := deleteOldEvents(ctx, db, a.id, cutoff)
		if err != nil {
			if log != nil {
				log.WithError(err).
					WithField("app_id", a.id).
					Error("retention: events delete failed")
			}
			return err
		}
		if log != nil && totalDeleted > 0 {
			log.WithField("app_id", a.id).
				WithField("events_deleted", totalDeleted).
				WithField("batches", batches).
				Info("retention: events cleanup complete")
		}
	}

	return nil
}

// dropOldPartitions finds connection_stats_YYYY_MM partitions whose entire
// month range falls before the cutoff derived from minRetentionDays, then
// detaches and drops them. Returns the count of dropped partitions.
func dropOldPartitions(ctx context.Context, db *pgxpool.Pool, minRetentionDays int, log *logrus.Logger) (int, error) {
	now := time.Now().UTC()
	// cutoffMonth is the first day of the month in which the cutoff falls.
	// Any partition entirely before cutoffMonth is safe to drop.
	cutoff := now.AddDate(0, 0, -minRetentionDays)
	cutoffMonth := time.Date(cutoff.Year(), cutoff.Month(), 1, 0, 0, 0, 0, time.UTC)

	prows, err := db.Query(ctx, `
		SELECT relname FROM pg_class
		WHERE relname LIKE 'connection_stats_%' AND relkind = 'r'
		ORDER BY relname
	`)
	if err != nil {
		return 0, fmt.Errorf("retention: query partitions: %w", err)
	}
	defer prows.Close()

	var partitions []string
	for prows.Next() {
		var name string
		if err := prows.Scan(&name); err != nil {
			return 0, fmt.Errorf("retention: scan partition name: %w", err)
		}
		partitions = append(partitions, name)
	}
	if err := prows.Err(); err != nil {
		return 0, fmt.Errorf("retention: iterate partitions: %w", err)
	}

	dropped := 0
	for _, name := range partitions {
		partMonth, err := parsePartitionMonth(name)
		if err != nil {
			// Not a YYYY_MM partition — skip.
			if log != nil {
				log.WithField("partition", name).Warn("retention: skipping unrecognised partition name")
			}
			continue
		}

		// The partition covers [partMonth, partMonth+1 month). It is entirely
		// before cutoffMonth only when partMonth < cutoffMonth.
		if !partMonth.Before(cutoffMonth) {
			continue
		}

		// DETACH then DROP. If already detached/dropped (idempotent run),
		// Postgres will error; we log and continue rather than failing the run.
		_, detachErr := db.Exec(ctx,
			fmt.Sprintf(`ALTER TABLE connection_stats DETACH PARTITION %s`, name),
		)
		if detachErr != nil {
			if log != nil {
				log.WithError(detachErr).WithField("partition", name).Warn("retention: detach partition failed, skipping")
			}
			continue
		}

		if _, dropErr := db.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, name)); dropErr != nil {
			if log != nil {
				log.WithError(dropErr).WithField("partition", name).Error("retention: drop partition failed")
			}
			return dropped, fmt.Errorf("retention: drop partition %s: %w", name, dropErr)
		}

		dropped++
		if log != nil {
			log.WithField("partition", name).Info("retention: partition dropped")
		}
	}

	return dropped, nil
}

// deleteOldEvents removes events rows older than cutoff that belong to the
// given app, in batches of batchSize to avoid long lock waits.
// Returns total rows deleted and the number of batches executed.
func deleteOldEvents(ctx context.Context, db *pgxpool.Pool, appID int64, cutoff time.Time) (int64, int, error) {
	var total int64
	batches := 0

	for {
		tag, err := db.Exec(ctx, `
			WITH to_delete AS (
				SELECT e.id
				FROM events e
				JOIN connections c   ON e.connection_id = c.id
				JOIN sessions s      ON c.session_id    = s.id
				JOIN participants p  ON s.participant_id = p.id
				JOIN conferences cf  ON p.conference_id  = cf.id
				WHERE cf.app_id = $1
				  AND e.ts < $2
				LIMIT $3
			)
			DELETE FROM events WHERE id IN (SELECT id FROM to_delete)
		`, appID, cutoff, batchSize)
		if err != nil {
			return total, batches, fmt.Errorf("retention: delete events for app %d: %w", appID, err)
		}

		n := tag.RowsAffected()
		if n == 0 {
			break
		}
		total += n
		batches++
	}

	return total, batches, nil
}

// parsePartitionMonth extracts the time.Time (first day of month, UTC) from a
// partition name of the form connection_stats_YYYY_MM.
func parsePartitionMonth(name string) (time.Time, error) {
	// Expected suffix: _YYYY_MM — e.g. "connection_stats_2025_11"
	suffix := strings.TrimPrefix(name, "connection_stats_")
	if suffix == name {
		return time.Time{}, fmt.Errorf("unexpected prefix in %q", name)
	}

	parts := strings.Split(suffix, "_")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("expected YYYY_MM in %q, got %d parts", name, len(parts))
	}

	year, err := strconv.Atoi(parts[0])
	if err != nil {
		return time.Time{}, fmt.Errorf("non-numeric year in %q", name)
	}
	month, err := strconv.Atoi(parts[1])
	if err != nil || month < 1 || month > 12 {
		return time.Time{}, fmt.Errorf("invalid month in %q", name)
	}

	return time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC), nil
}

// parseDailyCron parses a minimal daily cron expression "M H * * *" and
// returns the target hour and minute (UTC). Supports numeric minute and hour
// fields; the remaining three fields must be "*".
func parseDailyCron(cronStr string) (hour, minute int, err error) {
	fields := strings.Fields(cronStr)
	if len(fields) != 5 {
		return 0, 0, fmt.Errorf("expected 5 cron fields, got %d in %q", len(fields), cronStr)
	}

	minute, err = strconv.Atoi(fields[0])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("invalid minute field %q in cron %q", fields[0], cronStr)
	}
	hour, err = strconv.Atoi(fields[1])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("invalid hour field %q in cron %q", fields[1], cronStr)
	}

	return hour, minute, nil
}

// nextRunTime returns the next UTC time that matches hour:minute, strictly
// after from. If from is before today's target it returns today's target;
// otherwise it returns tomorrow's.
func nextRunTime(hour, minute int, from time.Time) time.Time {
	from = from.UTC()
	next := time.Date(from.Year(), from.Month(), from.Day(), hour, minute, 0, 0, time.UTC)
	if !next.After(from) {
		next = next.Add(24 * time.Hour)
	}
	return next
}
