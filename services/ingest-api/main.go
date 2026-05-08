package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/RTCMon/rtcmon/internal/cache"
	"github.com/RTCMon/rtcmon/internal/config"
	"github.com/RTCMon/rtcmon/internal/db"
	"github.com/RTCMon/rtcmon/internal/ingest"
	"github.com/RTCMon/rtcmon/internal/logger"
	"github.com/RTCMon/rtcmon/internal/migrate"
	"github.com/RTCMon/rtcmon/internal/ratelimit"
	"github.com/RTCMon/rtcmon/internal/worker"
	"github.com/RTCMon/rtcmon/services/ingest-api/server"
)

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

			// Create eMOS trigger: non-blocking send to a buffered channel.
			// The goroutine body is a stub; real eMOS computation is BE-017+.
			emosCh := make(chan int64, 256)
			go func() {
				for confID := range emosCh {
					log.WithField("conference_id", confID).Info("eMOS computation triggered (stub)")
				}
			}()
			emosTrigger := func(confID int64) {
				select {
				case emosCh <- confID:
				default:
					log.WithField("conference_id", confID).Warn("eMOS: channel full, dropping job")
				}
			}

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
			return serveWithShutdown(httpSrv, wp, emosCh, sigCh, 30*time.Second, log)
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
