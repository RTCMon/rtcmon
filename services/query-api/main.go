package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/RTCMon/rtcmon/internal/config"
	"github.com/RTCMon/rtcmon/internal/logger"
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

			// HTTP server wiring goes here in BE-017.
			return nil
		},
	}
}
