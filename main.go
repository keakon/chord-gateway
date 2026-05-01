// Package main is the chord gateway — a lightweight control plane
// that connects IM platforms (WeChat, Feishu, etc.) to chord headless
// processes via stdio JSON protocol.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/keakon/golog"
	"github.com/spf13/cobra"

	"github.com/keakon/chord-gateway/config"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	// Resolve paths first to get the default config file location.
	// Priority: --config flag > CHORD_GATEWAY_CONFIG env > $XDG_CONFIG_HOME/chord-gateway/config.yaml > ~/.config/chord-gateway/config.yaml
	paths, err := config.ResolveFromEnv("", "", "", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to resolve config paths: %v\n", err)
		os.Exit(1)
	}

	flagConfig := paths.ConfigFile

	rootCmd := &cobra.Command{
		Use:           "chord-gateway",
		Short:         "Chord remote control plane gateway",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runGateway(paths, &flagConfig),
	}

	rootCmd.Flags().StringVarP(&flagConfig, "config", "f", paths.ConfigFile, "Gateway config file path")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runGateway(paths *config.Paths, flagConfig *string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load(*flagConfig)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}

		// Validate configuration before starting any adapters/processes.
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("config validation: %w", err)
		}

		// Ensure state directory exists for logging.
		if err := os.MkdirAll(filepath.Dir(paths.LogFile), 0o755); err != nil {
			return fmt.Errorf("create log dir: %w", err)
		}

		// Set up logging to file with rotation, also mirror to stderr.
		logFile, err := golog.NewRotatingFileWriter(paths.LogFile, 10*1024*1024, 3)
		if err != nil {
			return fmt.Errorf("create rotating log writer: %w", err)
		}
		defer logFile.Close()
		writer := io.MultiWriter(os.Stderr, logFile)
		slog.SetDefault(slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{
			AddSource: true,
		})))

		activeIMs := cfg.ActiveIMs()
		slog.Info("chord-gateway starting",
			"config", *flagConfig,
			"state_dir", paths.StateDir,
			"ims", len(activeIMs),
			"workspaces", len(cfg.Workspaces),
			"idle_timeout", cfg.IdleTimeoutDuration(),
		)

		// Create the chord process manager
		mgr := NewChordManager(cfg, paths)

		// Create the notification router
		router := NewNotificationRouter(mgr)
		router.SetConfigFile(*flagConfig)

		// Create and start the IM adapter(s)
		var adapter IMAdapter
		if len(activeIMs) > 1 {
			adapter, err = NewMultiAdapter(cfg, paths, router)
			if err != nil {
				return fmt.Errorf("create multi-adapter: %w", err)
			}
		} else {
			adapter, err = NewIMAdapter(cfg, paths, router)
			if err != nil {
				return fmt.Errorf("create IM adapter: %w", err)
			}
		}

		// Wire the adapter into the router so notifications can be sent
		router.SetAdapter(adapter)

		slog.Info("gateway ready", "ims", len(activeIMs))

		// Start idle timeout checker
		go mgr.IdleCheckLoop()

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			slog.Info("gateway shutting down, terminating chord processes")
			mgr.StopAll(2 * time.Second)
			adapter.Disconnect()
		}()

		// Connect and block
		if err := adapter.Connect(); err != nil {
			return fmt.Errorf("IM adapter error: %w", err)
		}

		return nil
	}
}
