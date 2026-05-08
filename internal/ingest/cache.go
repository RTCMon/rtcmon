package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/RTCMon/rtcmon/internal/model"
)

const cacheTTL = 600 * time.Second

// redisClient abstracts Pipeline() so tests can inject a counting wrapper
// without implementing the full *redis.Client surface.
type redisClient interface {
	Pipeline() redis.Pipeliner
}

// updateCache writes one Redis hash per unique (AppID, ConferenceID) in the
// batch using a single pipeline round-trip.
//
// Key:    active_conf:{appId}:{conferenceId}
// Fields: participant_count (distinct UserIDs in batch), last_event_ts (max ts)
// TTL:    600 s, reset on every write.
//
// Redis errors are logged at warn level and never propagated — a Redis outage
// must not affect the DB flush path.
func (f *Flusher) updateCache(ctx context.Context, batch []model.IngestPayload) {
	type confStats struct {
		maxTS   int64
		userIDs map[string]struct{}
	}

	stats := make(map[string]*confStats)
	for _, p := range batch {
		key := fmt.Sprintf("%s:%s", p.AppID, p.ConferenceID)
		s := stats[key]
		if s == nil {
			s = &confStats{userIDs: make(map[string]struct{})}
			stats[key] = s
		}
		s.userIDs[p.UserID] = struct{}{}
		for _, ev := range p.Events {
			if ev.TS > s.maxTS {
				s.maxTS = ev.TS
			}
		}
	}

	if f.rdb == nil {
		return
	}
	pipe := f.rdb.Pipeline()
	for key, s := range stats {
		redisKey := "active_conf:" + key
		pipe.HSet(ctx, redisKey,
			"participant_count", len(s.userIDs),
			"last_event_ts", s.maxTS,
		)
		pipe.Expire(ctx, redisKey, cacheTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		f.log.WithError(err).Warn("ingest: redis cache update failed")
	}
}
