package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RTCMon/rtcmon/internal/cache"
	"github.com/RTCMon/rtcmon/internal/config"
	"github.com/RTCMon/rtcmon/internal/db"
	"github.com/RTCMon/rtcmon/internal/emos"
	"github.com/RTCMon/rtcmon/internal/ingest"
	"github.com/RTCMon/rtcmon/internal/logger"
	"github.com/RTCMon/rtcmon/internal/migrate"
	"github.com/RTCMon/rtcmon/internal/ratelimit"
	"github.com/RTCMon/rtcmon/internal/retention"
	"github.com/RTCMon/rtcmon/internal/stale"
	"github.com/RTCMon/rtcmon/internal/worker"
	"github.com/RTCMon/rtcmon/services/ingest-api/server"
)

// @title Ingest API
// @version 1.0
// @description RTCMon Ingest API
// @host localhost:8080
// @BasePath /

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ingest-api",
		Short: "WebRTC monitoring ingest API",
	}
	root.AddCommand(newServeCmd())
	root.AddCommand(newMigrateCmd())
	return root
}

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the ingest HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if cfg.Auth.JWTSecret == "" {
				return fmt.Errorf("AUTH_JWT_SECRET is required for the ingest API")
			}

			log := logger.New(cfg.Log.Level)
			log.WithField("port", cfg.Server.Port).Info("ingest-api starting")

			ctx := context.Background()

			// Initialize database pool
			pool, err := db.NewPool(ctx, cfg.DB)
			if err != nil {
				return fmt.Errorf("db init: %w", err)
			}
			defer pool.Close()

			// Initialize Redis client
			client, err := cache.NewClient(ctx, cfg.Redis)
			if err != nil {
				return fmt.Errorf("redis init: %w", err)
			}
			defer client.Close()

			// Create flusher and worker pool.
			flusher := ingest.New(pool, client, log, cfg.Worker.DeadLetterDir)
			wp := worker.New(
				worker.Config{
					WorkerCount:     cfg.Worker.Count,
					BatchSize:       cfg.Worker.BatchSize,
					FlushIntervalMs: cfg.Worker.FlushIntervalMs,
					ChannelCap:      cfg.Worker.ChannelCap,
				},
				flusher.Flush,
				log,
			)
			// wp.Shutdown() is called inside serveWithShutdown after signal.

			// Read configurable high-loss coefficient (RFC §3.7, K value).
			lossCoeff := emos.DefaultLossCoeff
			if v := os.Getenv("EMOS_LOSS_COEFF"); v != "" {
				if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
					lossCoeff = f
				} else {
					log.WithField("EMOS_LOSS_COEFF", v).Warn("invalid EMOS_LOSS_COEFF, using default")
				}
			}

			// Create eMOS trigger: non-blocking send to a buffered channel.
			// The goroutine processes conferences sequentially; close(emosCh) in
			// serveWithShutdown lets it drain and exit cleanly.
			emosCh := make(chan int64, 256)
			go func() {
				for confID := range emosCh {
					if err := emos.RunJob(context.Background(), pool, confID, lossCoeff, log); err != nil {
						log.WithError(err).WithField("conference_id", confID).Error("eMOS job failed")
					}
				}
			}()
			emosTrigger := func(confID int64) {
				select {
				case emosCh <- confID:
				default:
					log.WithField("conference_id", confID).Warn("eMOS: channel full, dropping job")
				}
			}

			// Stale-conference cleanup job — configurable interval and idle threshold.
			checkInterval := 5 * time.Minute
			if v := os.Getenv("STALE_CONF_CHECK_INTERVAL"); v != "" {
				if d, err := time.ParseDuration(v); err == nil && d > 0 {
					checkInterval = d
				} else {
					log.WithField("STALE_CONF_CHECK_INTERVAL", v).Warn("invalid STALE_CONF_CHECK_INTERVAL, using default 5m")
				}
			}
			idleThreshold := 15 * time.Minute
			if v := os.Getenv("STALE_CONF_IDLE_THRESHOLD"); v != "" {
				if d, err := time.ParseDuration(v); err == nil && d > 0 {
					idleThreshold = d
				} else {
					log.WithField("STALE_CONF_IDLE_THRESHOLD", v).Warn("invalid STALE_CONF_IDLE_THRESHOLD, using default 15m")
				}
			}
			staleJob := stale.New(pool, log, checkInterval, idleThreshold, lossCoeff, emosTrigger)
			staleJob.Start()

			// Data-retention cleanup job — configurable cron schedule.
			retentionCron := cfg.Retention.Cron
			if v := os.Getenv("RETENTION_JOB_CRON"); v != "" {
				retentionCron = v
			}
			retentionJob := retention.New(pool, log, retentionCron)
			retentionJob.Start()

			// Wire signal handling and HTTP server.
			rl := ratelimit.New(client, ratelimit.Config{
				Max:        cfg.RateLimit.Max,
				WindowSecs: cfg.RateLimit.WindowSecs,
			}, log)

			// Decode SERVER_MASTER_KEY (already validated by config.Load).
			var serverMasterKey []byte
			if cfg.Auth.ServerMasterKey != "" {
				serverMasterKey, _ = base64.StdEncoding.DecodeString(cfg.Auth.ServerMasterKey)
			}

			serverRL := ratelimit.New(client, ratelimit.Config{
				Max:        cfg.ServerRateLimit.Max,
				WindowSecs: cfg.ServerRateLimit.WindowSecs,
			}, log)

			srv := server.NewServer(ctx, pool, client, log, cfg.Auth.JWTSecret, rl, serverMasterKey, serverRL, wp.Enqueue, emosTrigger)

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

			httpSrv := &http.Server{
				Addr:    fmt.Sprintf(":%d", cfg.Server.Port),
				Handler: srv,
			}
			return serveWithShutdown(httpSrv, wp, staleJob, retentionJob, emosCh, sigCh, 30*time.Second, log)
		},
	}
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Database migration commands",
	}
	cmd.AddCommand(newMigrateUpCmd())
	cmd.AddCommand(newMigrateDownCmd())
	return cmd
}

func newMigrateUpCmd() *cobra.Command {
	var dbURL string

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dbURL == "" {
				dbURL = os.Getenv("DB_URL")
			}
			if dbURL == "" {
				return fmt.Errorf("database URL is required: set --db-url flag or DB_URL env var")
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Applying migrations...")
			if err := migrate.Up(cmd.Context(), dbURL); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Migrations applied successfully.")
			return nil
		},
	}

	cmd.Flags().StringVar(&dbURL, "db-url", "", "PostgreSQL connection URL (overrides DB_URL env var)")
	return cmd
}

func newMigrateDownCmd() *cobra.Command {
	var dbURL string
	var steps int

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Roll back N migration steps (default 1, 0 = all)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dbURL == "" {
				dbURL = os.Getenv("DB_URL")
			}
			if dbURL == "" {
				return fmt.Errorf("database URL is required: set --db-url flag or DB_URL env var")
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Rolling back %d migration step(s)...\n", steps)
			if err := migrate.Down(cmd.Context(), dbURL, steps); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Rollback complete.")
			return nil
		},
	}

	cmd.Flags().StringVar(&dbURL, "db-url", "", "PostgreSQL connection URL (overrides DB_URL env var)")
	cmd.Flags().IntVarP(&steps, "steps", "n", 1, "Number of migration steps to roll back (0 = all)")
	return cmd
}
