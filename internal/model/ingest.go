package model

// IngestPayload is the request body sent by the SDK on POST /v1/events.
type IngestPayload struct {
	ConferenceID string         `json:"conference_id"`
	SessionID    string         `json:"session_id"`
	ConnectionID string         `json:"connection_id"`
	Events       []StatSnapshot `json:"events"`
}
