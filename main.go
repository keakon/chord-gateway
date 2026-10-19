// Package main is the chord gateway — a lightweight control plane
// that connects IM platforms (WeChat, Feishu, etc.) to chord headless
// processes via stdio JSON protocol.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/keakon/golog"
	"github.com/keakon/golog/log"
	"github.com/spf13/cobra"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/buildinfo"
)

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

	rootCmd.PersistentFlags().StringVarP(flagConfig, "config", "f", paths.ConfigFile, "Gateway config file path")
	rootCmd.Flags().Bool("pair", false, "Open a 10-minute private-chat pairing window for unregistered platforms")
	rootCmd.Flags().Bool("console", false, "Use local stdin/stdout without connecting IM platforms (requires one workspace)")
	rootCmd.MarkFlagsMutuallyExclusive("pair", "console")
	rootCmd.AddCommand(newPairingCommand(paths, flagConfig))
	return rootCmd
}

// checkPairedOwnerRequirement enforces the pairing prerequisite for platforms
// that declare chat bindings. Bindings stay dormant while a platform has no
// paired owner (admission only accepts code-verified private /bind), so with
// --pair the gateway can start and complete pairing; without --pair those
// bindings could never activate and the user gets actionable guidance instead.
func checkPairedOwnerRequirement(cfg *config.Config, pair bool) error {
	for _, im := range cfg.ActiveIMs() {
		switch {
		case im.OwnerIdentity() != nil:
			// Paired; bindings are live.
		case len(im.Bindings()) > 0 && pair:
			log.Warnf("ims.%s: chat_bindings stay inactive until an owner is paired", im.Type())
		case len(im.Bindings()) > 0:
			return fmt.Errorf("ims.%s: chat_bindings requires a paired owner; restart with --pair, then send /bind \"<directory>\" <pairing_code> in a private chat (the pairing code is shown on the terminal), or set owner: {account_id: <bot account id>, user_id: <your user id>} in the config", im.Type())
		case !pair:
			log.Warnf("ims.%s: no paired owner; start with --pair to pair this platform", im.Type())
		}
	}
	return nil
}

func runGateway(paths *config.Paths, flagConfig *string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {

		store, locks, err := loadLockedConfiguration(*flagConfig, paths)
		if err != nil {
			return err
		}
		if locks != nil {
			defer locks.Close()
		}
		cfg := store.Snapshot()
		console, _ := cmd.Flags().GetBool("console")
		var ingressStore *IngressStore
		if !console {
			ingressStore = NewIngressStore()
			// Stop workers before releasing their process-local state.
			defer ingressStore.Close()
		}
		if console && len(cfg.Workspaces) != 1 {
			return fmt.Errorf("console requires exactly one workspace")
		}
		pair, _ := cmd.Flags().GetBool("pair")
		var terminal *os.File
		needsTerminal := false
		for _, im := range cfg.ActiveIMs() {
			if im.OwnerIdentity() == nil {
				needsTerminal = true
			}
		}
		if pair && needsTerminal {
			terminal, err = openPairingTerminal()
			if err != nil {
				return fmt.Errorf("pairing requires a controlling terminal: %w", err)
			}
			defer terminal.Close()
		}

		if err := os.MkdirAll(paths.StateDir, privateDirMode); err != nil {
			return fmt.Errorf("create state dir: %w", err)
		}
		if err := os.Chmod(paths.StateDir, privateDirMode); err != nil {
			return fmt.Errorf("restrict state dir permissions: %w", err)
		}

		// Resource locks are held before opening the log. Replay diagnostics
		// from the locked configuration after both log outputs are installed.
		logFile, err := newPrivateRotatingLogWriter(paths.LogFile, 10*1024*1024, 3)
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
		for _, message := range storeWarningMessages(store) {
			log.Warnf("%s", message)
		}
		// Console mode never connects IM platforms, so the chat-binding pairing
		// prerequisite (and its --pair guidance, which is mutually exclusive with
		// --console) does not apply.
		if !console {
			if err := checkPairedOwnerRequirement(cfg, pair); err != nil {
				return err
			}
		}

		if pair && !needsTerminal {
			log.Infof("no platform requires pairing; existing owners are unchanged")
		}

		logEffectiveProxies(cfg)

		activeIMs := cfg.ActiveIMs()
		log.Infof("chord-gateway starting %s config=%v state_dir=%v ims=%v workspaces=%v idle_timeout=%v", buildinfo.Current().LogString(), *flagConfig,
			paths.StateDir,
			len(activeIMs),
			len(cfg.Workspaces),
			cfg.IdleTimeoutDuration(),
		)

		// Create the chord process manager
		mgr := NewChordManager(cfg, paths)
		mgr.store = store

		// Create the notification router
		router := NewNotificationRouter(mgr)
		router.admission = NewAdmission(NewPairingService(store, pair, terminal))
		mgr.admission = router.admission
		// Show the universal pairing code immediately at startup, before any
		// platform connects, so it is valid across all platforms for the window.
		if terminal != nil {
			if err := router.admission.pairing.BeginPairing(); err != nil {
				return fmt.Errorf("cannot begin pairing: %w", err)
			}
		}
		var ingress *IngressService
		if ingressStore != nil {
			ingress = NewIngressService(ingressStore, router)
			defer ingress.Close()
			if err := ingress.Start(context.Background()); err != nil {
				return fmt.Errorf("start IM ingress: %w", err)
			}
		}

		// Create and start the IM adapter(s)
		var adapter IMAdapter
		if console {
			adapter = newConsoleAdapter(router)
		} else if len(activeIMs) > 1 {
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
		metricsCtx, cancelMetrics := context.WithCancel(context.Background())
		metricsDone := make(chan struct{})
		go func() {
			defer close(metricsDone)
			reportQueueMetrics(metricsCtx, router, adapter, queueMetricsLogInterval, nil)
		}()
		defer func() {
			cancelMetrics()
			<-metricsDone
		}()
		var shutdownOnce sync.Once
		shutdown := func() {
			shutdownOnce.Do(func() {
				if ingress != nil {
					ingress.Close()
				}
				shutdownGateway(adapter, mgr, router, 2*time.Second)
			})
		}
		shutdownDone := make(chan struct{})
		defer func() {
			stop()
			<-shutdownDone
		}()
		go func() {
			defer close(shutdownDone)
			select {
			case <-ctx.Done():
			case <-store.Fatal():
				stop()
			}
			log.Infof("gateway shutting down, terminating chord processes")
			shutdown()
		}()

		// Connect and block
		if err := adapter.Connect(); err != nil {
			return fmt.Errorf("IM adapter error: %w", err)
		}

		return nil
	}
}

func shutdownGateway(adapter IMAdapter, mgr *ChordManager, router *NotificationRouter, grace time.Duration) {
	if mgr != nil && mgr.store != nil {
		mgr.store.closing.Store(true)
	}
	if router != nil && router.admission != nil {
		router.admission.Close()
	}
	if mgr != nil {
		mgr.StopAll(grace)
		mgr.WaitInputReceiptsShutdown()
	}
	if router != nil {
		router.Close()
	}
	if adapter != nil {
		adapter.Disconnect()
	}
}
