package config

import (
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Server          ServerConfig
	DB              DBConfig
	Redis           RedisConfig
	Auth            AuthConfig
	Session         SessionConfig
	RateLimit       RateLimitConfig
	ServerRateLimit RateLimitConfig `mapstructure:"server_rate_limit"`
	Worker          WorkerConfig
	Log             LogConfig
}

type ServerConfig struct {
	Port        int `mapstructure:"port"`
	MetricsPort int `mapstructure:"metrics_port"`
}

type DBConfig struct {
	URL          string `mapstructure:"url"`
	MaxConns     int    `mapstructure:"max_conns"`
	MinConns     int    `mapstructure:"min_conns"`
	ConnLifetime string `mapstructure:"conn_lifetime"`
}

type RedisConfig struct {
	URL      string `mapstructure:"url"`
	PoolSize int    `mapstructure:"pool_size"`
}

type AuthConfig struct {
	JWTSecret       string `mapstructure:"jwt_secret"`
	ServerMasterKey string `mapstructure:"server_master_key"`
}

type RateLimitConfig struct {
	Max        int `mapstructure:"max"`
	WindowSecs int `mapstructure:"window_secs"`
}

type WorkerConfig struct {
	Count           int    `mapstructure:"count"`
	BatchSize       int    `mapstructure:"batch_size"`
	FlushIntervalMs int    `mapstructure:"flush_interval_ms"`
	ChannelCap      int    `mapstructure:"channel_cap"`
	DeadLetterDir   string `mapstructure:"dead_letter_dir"`
}

type SessionConfig struct {
	TTLSeconds int `mapstructure:"ttl_seconds"`
}

type LogConfig struct {
	Level string `mapstructure:"level"`
}

// Load reads configuration from a YAML file (config.yaml in the working
// directory, if present) and from environment variables. Environment variables
// take precedence and use the pattern SECTION_KEY (e.g. SERVER_PORT,
// DB_URL, REDIS_URL).
func Load() (*Config, error) {
	v := viper.New()

	setDefaults(v)

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")

	// Env vars override YAML. Map SERVER_PORT → server.port via replacer.
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Explicitly bind env vars to ensure they're read.
	v.BindEnv("auth.jwt_secret", "AUTH_JWT_SECRET")
	v.BindEnv("auth.server_master_key", "SERVER_MASTER_KEY")

	if err := v.ReadInConfig(); err != nil {
		// Missing config file is not an error; all values can come from env.
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("config: read file: %w", err)
		}
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	applyComputedDefaults(cfg)

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.metrics_port", 9090)

	v.SetDefault("db.max_conns", 20)
	v.SetDefault("db.min_conns", 5)
	v.SetDefault("db.conn_lifetime", "30m")

	v.SetDefault("redis.pool_size", 10)

	v.SetDefault("rate_limit.max", 100)
	v.SetDefault("rate_limit.window_secs", 60)

	v.SetDefault("server_rate_limit.max", 1000)
	v.SetDefault("server_rate_limit.window_secs", 60)

	v.SetDefault("worker.batch_size", 500)
	v.SetDefault("worker.flush_interval_ms", 100)
	v.SetDefault("worker.dead_letter_dir", "./dead_letter/")

	v.SetDefault("session.ttl_seconds", 28800) // 8 hours

	v.SetDefault("log.level", "info")
}

// applyComputedDefaults sets fields whose defaults depend on runtime values
// (e.g. CPU count) after unmarshalling, so they do not appear in viper's
// static default map.
func applyComputedDefaults(cfg *Config) {
	if cfg.Worker.Count == 0 {
		cfg.Worker.Count = runtime.NumCPU() * 2
	}

	if cfg.Worker.ChannelCap == 0 {
		cfg.Worker.ChannelCap = cfg.Worker.Count * cfg.Worker.BatchSize * 4
	}
}

// validate checks that required configuration values are set.
// AUTH_JWT_SECRET is validated here only when present; services that require
// it (ingest-api) must perform their own startup check.
func validate(cfg *Config) error {
	if cfg.Auth.ServerMasterKey != "" {
		decoded, err := base64.StdEncoding.DecodeString(cfg.Auth.ServerMasterKey)
		if err != nil {
			return fmt.Errorf("auth.server_master_key: invalid base64: %w", err)
		}
		if len(decoded) != 32 {
			return fmt.Errorf("auth.server_master_key: must decode to exactly 32 bytes, got %d", len(decoded))
		}
	}

	return nil
}
