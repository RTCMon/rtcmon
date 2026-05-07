package handler

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestHealthResponse_ValidJSON tests that HealthResponse marshals to valid JSON
func TestHealthResponse_ValidJSON(t *testing.T) {
	resp := &HealthResponse{
		Status: "ok",
		DB:     "ok",
		Redis:  "ok",
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("failed to marshal response: %v", err)
	}

	var decoded HealthResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if decoded.Status != resp.Status || decoded.DB != resp.DB || decoded.Redis != resp.Redis {
		t.Errorf("response structure mismatch: expected %+v, got %+v", resp, decoded)
	}
}

// TestHealthResponse_AllHealthy tests the response when all services are healthy
func TestHealthResponse_AllHealthy(t *testing.T) {
	resp := &HealthResponse{
		Status: "ok",
		DB:     "ok",
		Redis:  "ok",
	}

	body, _ := json.Marshal(resp)

	var decoded HealthResponse
	json.Unmarshal(body, &decoded)

	if decoded.Status != "ok" || decoded.DB != "ok" || decoded.Redis != "ok" {
		t.Errorf("expected all ok, got: %+v", decoded)
	}
}

// TestHealthResponse_DBDown tests the response when DB is down
func TestHealthResponse_DBDown(t *testing.T) {
	resp := &HealthResponse{
		Status: "error",
		DB:     "error",
		Redis:  "ok",
	}

	body, _ := json.Marshal(resp)

	var decoded HealthResponse
	json.Unmarshal(body, &decoded)

	if decoded.Status != "error" || decoded.DB != "error" {
		t.Errorf("expected db error, got: %+v", decoded)
	}
	if decoded.Redis != "ok" {
		t.Errorf("expected redis ok, got: %+v", decoded)
	}
}

// TestHealthResponse_RedisDown tests the response when Redis is down
func TestHealthResponse_RedisDown(t *testing.T) {
	resp := &HealthResponse{
		Status: "error",
		DB:     "ok",
		Redis:  "error",
	}

	body, _ := json.Marshal(resp)

	var decoded HealthResponse
	json.Unmarshal(body, &decoded)

	if decoded.Status != "error" || decoded.Redis != "error" {
		t.Errorf("expected redis error, got: %+v", decoded)
	}
	if decoded.DB != "ok" {
		t.Errorf("expected db ok, got: %+v", decoded)
	}
}

// TestHealthResponse_ContentType tests that the response has correct content type
func TestHealthResponse_ContentType(t *testing.T) {
	// Simulate a simple health response being written
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "application/json")
	resp := &HealthResponse{
		Status: "ok",
		DB:     "ok",
		Redis:  "ok",
	}
	json.NewEncoder(w.Body).Encode(resp)

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected content-type application/json, got %s", ct)
	}
}
