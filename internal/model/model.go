package model

import (
	"encoding/json"
	"time"
)

type Organization struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type App struct {
	ID                int64           `json:"id"`
	OrgID             int64           `json:"org_id"`
	Name              string          `json:"name"`
	APIKeyHash        string          `json:"-"`
	RetentionDays     int             `json:"retention_days"`
	ObservationConfig json.RawMessage `json:"observation_config,omitempty"`
}

type Conference struct {
	ID               int64      `json:"id"`
	AppID            int64      `json:"app_id"`
	ExternalID       string     `json:"external_id"`
	StartedAt        time.Time  `json:"started_at"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	ParticipantCount int        `json:"participant_count"`
}

type Participant struct {
	ID           int64      `json:"id"`
	ConferenceID int64      `json:"conference_id"`
	UserID       string     `json:"user_id"`
	DisplayName  string     `json:"display_name"`
	JoinedAt     time.Time  `json:"joined_at"`
	LeftAt       *time.Time `json:"left_at,omitempty"`
}

type Session struct {
	ID            int64  `json:"id"`
	ParticipantID int64  `json:"participant_id"`
	Browser       string `json:"browser"`
	OS            string `json:"os"`
	Country       string `json:"country"`
	City          string `json:"city"`
	NetworkType   string `json:"network_type"`
	SDKVersion    string `json:"sdk_version"`
}

type Connection struct {
	ID         int64      `json:"id"`
	SessionID  int64      `json:"session_id"`
	PeerID     string     `json:"peer_id"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	ICEState   string     `json:"ice_state"`
	DTLSState  string     `json:"dtls_state"`
	CodecAudio string     `json:"codec_audio"`
	CodecVideo string     `json:"codec_video"`
}

// ICECandidateType holds the local/remote candidate types and transport protocol
// for the nominated ICE candidate pair on a connection.
type ICECandidateType struct {
	Local    string `json:"local"`
	Remote   string `json:"remote"`
	Protocol string `json:"protocol"`
}

type StatSnapshot struct {
	TS           int64  `json:"ts"`
	ConnectionID string `json:"connection_id"`

	// Network
	RTTMs                  float64 `json:"rtt_ms"`
	JitterMs               float64 `json:"jitter_ms"`
	PacketLossRate         float64 `json:"packet_loss_rate"`
	BitrateInKbps          float64 `json:"bitrate_in_kbps"`
	BitrateOutKbps         float64 `json:"bitrate_out_kbps"`
	PacketRateIn           float64 `json:"packet_rate_in"`
	PacketRateOut          float64 `json:"packet_rate_out"`
	AvailableBandwidthKbps float64 `json:"available_bandwidth_kbps"`

	// Buffer / retransmission
	NACKCountDelta            int     `json:"nack_count_delta"`
	PLICountDelta             int     `json:"pli_count_delta"`
	FIRCountDelta             int     `json:"fir_count_delta"`
	RetransmittedPacketsDelta int     `json:"retransmitted_packets_delta"`
	TargetBitrateKbps         float64 `json:"target_bitrate_kbps"`

	// Audio
	AudioLevel       float64 `json:"audio_level"`
	LocalAudioLevel  float64 `json:"local_audio_level"`
	ConcealmentRatio float64 `json:"concealment_ratio"`

	// Video
	FPS             float64 `json:"fps"`
	FrameWidth      int     `json:"frame_width"`
	FrameHeight     int     `json:"frame_height"`
	SendFPS         float64 `json:"send_fps"`
	SendFrameWidth  int     `json:"send_frame_width"`
	SendFrameHeight int     `json:"send_frame_height"`
	FreezeCount     int     `json:"freeze_count"`
	FreezeDurationMs float64 `json:"freeze_duration_ms"`
	FrameDropRate   float64 `json:"frame_drop_rate"`
	// QualityLimitReason is one of: "none", "bandwidth", "cpu", "other"
	QualityLimitReason string `json:"quality_limit_reason"`

	// ICE
	ICECandidateType *ICECandidateType `json:"ice_candidate_type,omitempty"`
}

type Event struct {
	ID           int64           `json:"id"`
	ConnectionID *int64          `json:"connection_id,omitempty"`
	SessionID    *int64          `json:"session_id,omitempty"`
	TS           time.Time       `json:"ts"`
	EventType    string          `json:"event_type"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

// SessionQuality holds the computed eMOS score and outcome for a session.
// Outcome is one of: "success" (eMOS >= 3.5), "degraded" (2.0–3.5), "failed" (< 2.0).
type SessionQuality struct {
	SessionID  int64     `json:"session_id"`
	EMOS       float32   `json:"emos"`
	Outcome    string    `json:"outcome"`
	ComputedAt time.Time `json:"computed_at"`
}
