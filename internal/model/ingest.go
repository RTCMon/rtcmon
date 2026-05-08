package model

// IngestPayload is the request body sent by the SDK on POST /v1/events or
// POST /v1/server/events. AppID and UserID are populated from JWT claims (or
// from DB lookup for HMAC auth) by the handler; they are not part of the wire
// format. Source is set to "server" by the server events handler to record
// provenance.
type IngestPayload struct {
	AppID        string         `json:"app_id,omitempty"`
	UserID       string         `json:"user_id,omitempty"`
	Source       string         `json:"source,omitempty"`
	ConferenceID string         `json:"conference_id"`
	SessionID    string         `json:"session_id"`
	ConnectionID string         `json:"connection_id"`
	Events       []StatSnapshot `json:"events"`
}
