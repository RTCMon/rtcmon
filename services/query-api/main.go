package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/RTCMon/rtcmon/internal/cache"
	"github.com/RTCMon/rtcmon/internal/config"
	"github.com/RTCMon/rtcmon/internal/db"
	"github.com/RTCMon/rtcmon/internal/logger"
	"github.com/RTCMon/rtcmon/internal/session"
	"github.com/RTCMon/rtcmon/services/query-api/server"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "query-api",
		Short: "WebRTC monitoring query API",
	}
	root.AddCommand(newServeCmd())
	return root
}

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the query HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			log := logger.New(cfg.Log.Level)
			log.WithField("port", cfg.Server.Port).Info("query-api starting")

			ctx := context.Background()

			pool, err := db.NewPool(ctx, cfg.DB)
			if err != nil {
				return fmt.Errorf("db init: %w", err)
			}
			defer pool.Close()

			rdb, err := cache.NewClient(ctx, cfg.Redis)
			if err != nil {
				return fmt.Errorf("redis init: %w", err)
			}
			defer rdb.Close()

			sessions := session.NewStore(rdb, cfg.Session.TTLSeconds)

			var serverMasterKey []byte
			if cfg.Auth.ServerMasterKey != "" {
				serverMasterKey, _ = base64.StdEncoding.DecodeString(cfg.Auth.ServerMasterKey)
			}

			srv := server.NewServer(ctx, pool, rdb, log, sessions, serverMasterKey)

			log.WithField("port", cfg.Server.Port).Info("query-api listening")
			return http.ListenAndServe(fmt.Sprintf(":%d", cfg.Server.Port), srv)
		},
	}
}
