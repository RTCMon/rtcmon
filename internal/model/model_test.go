package model_test

import (
	"encoding/json"
	"testing"

	. "github.com/RTCMon/rtcmon/internal/model"
)

func TestIngestPayloadJSONRoundTrip(t *testing.T) {
	original := IngestPayload{
		ConferenceID: "conf-123",
		SessionID:    "sess-456",
		ConnectionID: "conn-789",
		Events: []StatSnapshot{
			{
				TS:             1700000000000,
				ConnectionID:   "conn-789",
				RTTMs:          45.5,
				JitterMs:       3.2,
				PacketLossRate: 0.01,
				BitrateInKbps:  512,
				BitrateOutKbps: 256,
				FPS:            30,
				AudioLevel:     0.75,
				FreezeCount:    0,
				QualityLimitReason: "none",
				ICECandidateType: &ICECandidateType{
					Local:    "host",
					Remote:   "srflx",
					Protocol: "udp",
				},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded IngestPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ConferenceID != original.ConferenceID {
		t.Errorf("ConferenceID: got %q, want %q", decoded.ConferenceID, original.ConferenceID)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID: got %q, want %q", decoded.SessionID, original.SessionID)
	}
	if decoded.ConnectionID != original.ConnectionID {
		t.Errorf("ConnectionID: got %q, want %q", decoded.ConnectionID, original.ConnectionID)
	}
	if len(decoded.Events) != 1 {
		t.Fatalf("Events length: got %d, want 1", len(decoded.Events))
	}

	ev := decoded.Events[0]
	if ev.TS != original.Events[0].TS {
		t.Errorf("Events[0].TS: got %d, want %d", ev.TS, original.Events[0].TS)
	}
	if ev.RTTMs != original.Events[0].RTTMs {
		t.Errorf("Events[0].RTTMs: got %v, want %v", ev.RTTMs, original.Events[0].RTTMs)
	}
	if ev.ICECandidateType == nil {
		t.Fatal("Events[0].ICECandidateType: got nil, want non-nil")
	}
	if ev.ICECandidateType.Protocol != "udp" {
		t.Errorf("ICECandidateType.Protocol: got %q, want %q", ev.ICECandidateType.Protocol, "udp")
	}
}

func TestStatSnapshotZeroValue(t *testing.T) {
	var s StatSnapshot
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal zero-value StatSnapshot: %v", err)
	}

	var decoded StatSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal zero-value StatSnapshot: %v", err)
	}

	if decoded.ICECandidateType != nil {
		t.Errorf("ICECandidateType: expected nil on zero-value, got %+v", decoded.ICECandidateType)
	}
}
