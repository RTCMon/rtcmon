package model

// IngestPayload is the request body sent by the SDK on POST /v1/events.
// AppID and UserID are populated from JWT claims by the handler (BE-013);
// they are not part of the SDK wire format.
type IngestPayload struct {
	AppID        string         `json:"app_id,omitempty"`
	UserID       string         `json:"user_id,omitempty"`
	ConferenceID string         `json:"conference_id"`
	SessionID    string         `json:"session_id"`
	ConnectionID string         `json:"connection_id"`
	Events       []StatSnapshot `json:"events"`
}
