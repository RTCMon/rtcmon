package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
	"github.com/RTCMon/rtcmon/internal/model"
)

const maxEvents = 200

// HandleEvents decodes and validates the POST /v1/events request body.
// Auth is enforced by the router-level middleware; this handler only sees
// requests that have already passed JWT verification. It returns 202 on
// success and 400 with a JSON error body on validation failure.
func HandleEvents(log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Claims are available for downstream use (e.g. scoping to app_id).
		// ClaimsFromContext never panics — returns zero-value when absent.
		claims := auth.ClaimsFromContext(r.Context())
		log.WithField("app_id", claims.AppID).Debug("events: request received")

		var payload model.IngestPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeEventError(w, http.StatusBadRequest, "invalid JSON")
			return
		}

		if err := validatePayload(&payload); err != nil {
			writeEventError(w, http.StatusBadRequest, err.Error())
			return
		}

		w.WriteHeader(http.StatusAccepted)
	}
}

// validatePayload checks all required fields and per-event constraints.
func validatePayload(p *model.IngestPayload) error {
	if p.ConferenceID == "" {
		return fmt.Errorf("conference_id is required")
	}
	if p.SessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	if p.ConnectionID == "" {
		return fmt.Errorf("connection_id is required")
	}
	if len(p.Events) == 0 {
		return fmt.Errorf("events must not be empty")
	}
	if len(p.Events) > maxEvents {
		return fmt.Errorf("events exceeds maximum of %d", maxEvents)
	}

	for i, ev := range p.Events {
		if ev.TS <= 0 {
			return fmt.Errorf("events[%d]: ts must be a positive Unix millisecond timestamp", i)
		}
		if err := validateStatSnapshot(&ev, i); err != nil {
			return err
		}
	}

	return nil
}

// validateStatSnapshot checks that all numeric fields in a StatSnapshot are ≥ 0.
func validateStatSnapshot(s *model.StatSnapshot, idx int) error {
	fields := []struct {
		name  string
		value float64
	}{
		{"rtt_ms", s.RTTMs},
		{"jitter_ms", s.JitterMs},
		{"packet_loss_rate", s.PacketLossRate},
		{"bitrate_in_kbps", s.BitrateInKbps},
		{"bitrate_out_kbps", s.BitrateOutKbps},
		{"packet_rate_in", s.PacketRateIn},
		{"packet_rate_out", s.PacketRateOut},
		{"available_bandwidth_kbps", s.AvailableBandwidthKbps},
		{"audio_level", s.AudioLevel},
		{"local_audio_level", s.LocalAudioLevel},
		{"concealment_ratio", s.ConcealmentRatio},
		{"fps", s.FPS},
		{"send_fps", s.SendFPS},
		{"freeze_duration_ms", s.FreezeDurationMs},
		{"frame_drop_rate", s.FrameDropRate},
		{"target_bitrate_kbps", s.TargetBitrateKbps},
	}

	for _, f := range fields {
		if f.value < 0 {
			return fmt.Errorf("events[%d]: %s must be >= 0", idx, f.name)
		}
	}

	return nil
}

func writeEventError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}
