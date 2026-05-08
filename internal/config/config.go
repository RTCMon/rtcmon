package config

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Server ServerConfig
	DB     DBConfig
	Redis  RedisConfig
	Auth      AuthConfig
	RateLimit RateLimitConfig
	Worker    WorkerConfig
	Log    LogConfig
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
	JWTSecret string `mapstructure:"jwt_secret"`
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

	// Explicitly bind env var to ensure it's read
	v.BindEnv("auth.jwt_secret", "AUTH_JWT_SECRET")

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

	v.SetDefault("worker.batch_size", 500)
	v.SetDefault("worker.flush_interval_ms", 100)
	v.SetDefault("worker.dead_letter_dir", "./dead_letter/")

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
func validate(cfg *Config) error {
	if cfg.Auth.JWTSecret == "" {
		return fmt.Errorf("auth.jwt_secret is required: set AUTH_JWT_SECRET environment variable")
	}
	return nil
}
