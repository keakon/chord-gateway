// Package main is the chord gateway — a lightweight control plane
// that connects IM platforms (WeChat, Feishu, etc.) to chord headless
// processes via stdio JSON protocol.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/keakon/golog"
	"github.com/keakon/golog/log"
	"github.com/spf13/cobra"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/buildinfo"
)

// version is the bare gateway version string injected via ldflags.
//
// The CLI prints the richer build identity from buildinfo.Current().Short(),
// but we still keep this historical variable as the compatibility point for
// existing `-X main.version=...` build pipelines. init() mirrors whichever side
// was set so the CLI version output, startup logs, and diagnostics all agree.
//
// Historical path:
//
//	-ldflags "-X main.version=<version>"
//
// Newer builds may also (or instead) override internal/buildinfo.Version
// directly, together with Commit, BuildTime, and Dirty for richer diagnostics:
//
//	-ldflags "-X github.com/keakon/chord-gateway/internal/buildinfo.Version=<version> ..."
var version = "dev"

func init() {
	// init() runs after package-level var initialization for both this file
	// and internal/buildinfo, but before main() — and before anything calls
	// buildinfo.Current() (which is sync.OnceValue-cached). This is the
	// correct time to bridge the two ldflags paths.
	switch {
	case version != "dev" && buildinfo.Version == "dev":
		// Only the historical -X main.version=... path was used.
		buildinfo.Version = version
	case version == "dev" && buildinfo.Version != "dev":
		// Only the new -X .../buildinfo.Version=... path was used.
		version = buildinfo.Version
	}
	// If both are set, we trust each — CI may set them deliberately and the
	// values are expected to match.
}

func main() {
	// Resolve paths first to get the default config file location.
	// Priority: --config flag > CHORD_GATEWAY_CONFIG env > $XDG_CONFIG_HOME/chord-gateway/config.yaml > ~/.config/chord-gateway/config.yaml
	paths, err := config.ResolveFromEnv("", "", "", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to resolve config paths: %v\n", err)
		os.Exit(1)
	}

	flagConfig := paths.ConfigFile
	rootCmd := newRootCmd(paths, &flagConfig)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newRootCmd(paths *config.Paths, flagConfig *string) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "chord-gateway",
		Short:         "Chord remote control plane gateway",
		Version:       buildinfo.Current().Short(),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runGateway(paths, flagConfig),
	}

	rootCmd.Flags().StringVarP(flagConfig, "config", "f", paths.ConfigFile, "Gateway config file path")
	return rootCmd
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

		// Set up golog to write to stderr and to a rotating log file.
		logFile, err := golog.NewRotatingFileWriter(paths.LogFile, 10*1024*1024, 3)
		if err != nil {
			return fmt.Errorf("create rotating log writer: %w", err)
		}
		formatter := golog.ParseFormat("[%l %D %T %s] %m")
		handler := golog.NewHandler(golog.InfoLevel, formatter)
		handler.AddWriter(golog.NewStderrWriter())
		handler.AddWriter(logFile)
		logger := golog.NewLogger(golog.InfoLevel)
		logger.AddHandler(handler)
		defer logger.Close()
		log.SetDefaultLogger(logger)

		activeIMs := cfg.ActiveIMs()
		log.Infof("chord-gateway starting %s config=%v state_dir=%v ims=%v workspaces=%v idle_timeout=%v", buildinfo.Current().LogString(), *flagConfig,
			paths.StateDir,
			len(activeIMs),
			len(cfg.Workspaces),
			cfg.IdleTimeoutDuration(),
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

		log.Infof("gateway ready ims=%v", len(activeIMs))

		// Start idle timeout checker
		go mgr.IdleCheckLoop()

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			log.Infof("gateway shutting down, terminating chord processes")
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
