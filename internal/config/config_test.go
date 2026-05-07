package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RTCMon/rtcmon/internal/config"
)

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("Server.Port: got %d, want 8080", cfg.Server.Port)
	}
	if cfg.Server.MetricsPort != 9090 {
		t.Errorf("Server.MetricsPort: got %d, want 9090", cfg.Server.MetricsPort)
	}
	if cfg.DB.MaxConns != 20 {
		t.Errorf("DB.MaxConns: got %d, want 20", cfg.DB.MaxConns)
	}
	if cfg.DB.MinConns != 5 {
		t.Errorf("DB.MinConns: got %d, want 5", cfg.DB.MinConns)
	}
	if cfg.DB.ConnLifetime != "30m" {
		t.Errorf("DB.ConnLifetime: got %q, want 30m", cfg.DB.ConnLifetime)
	}
	if cfg.Redis.PoolSize != 10 {
		t.Errorf("Redis.PoolSize: got %d, want 10", cfg.Redis.PoolSize)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level: got %q, want info", cfg.Log.Level)
	}
	if cfg.Worker.Count == 0 {
		t.Error("Worker.Count: expected non-zero computed default")
	}
	if cfg.Worker.ChannelCap == 0 {
		t.Error("Worker.ChannelCap: expected non-zero computed default")
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("SERVER_PORT", "9999")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("DB_MAX_CONNS", "50")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Port != 9999 {
		t.Errorf("Server.Port: got %d, want 9999", cfg.Server.Port)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("Log.Level: got %q, want debug", cfg.Log.Level)
	}
	if cfg.DB.MaxConns != 50 {
		t.Errorf("DB.MaxConns: got %d, want 50", cfg.DB.MaxConns)
	}
}

func TestLoad_YAMLFile(t *testing.T) {
	clearEnv(t)

	dir := t.TempDir()
	yaml := `
server:
  port: 7777
  metrics_port: 8888
log:
  level: warn
db:
  max_conns: 30
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	// Change to temp dir so viper finds config.yaml there.
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Port != 7777 {
		t.Errorf("Server.Port: got %d, want 7777", cfg.Server.Port)
	}
	if cfg.Server.MetricsPort != 8888 {
		t.Errorf("Server.MetricsPort: got %d, want 8888", cfg.Server.MetricsPort)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("Log.Level: got %q, want warn", cfg.Log.Level)
	}
	if cfg.DB.MaxConns != 30 {
		t.Errorf("DB.MaxConns: got %d, want 30", cfg.DB.MaxConns)
	}
}

// clearEnv removes env keys that could bleed across tests.
func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"SERVER_PORT", "SERVER_METRICS_PORT",
		"DB_URL", "DB_MAX_CONNS", "DB_MIN_CONNS", "DB_CONN_LIFETIME",
		"REDIS_URL", "REDIS_POOL_SIZE",
		"AUTH_JWT_SECRET",
		"WORKER_COUNT", "WORKER_BATCH_SIZE", "WORKER_FLUSH_INTERVAL_MS", "WORKER_CHANNEL_CAP",
		"LOG_LEVEL",
	}
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}
