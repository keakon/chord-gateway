package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/keakon/chord-gateway/config"
)

func newPairingCommand(paths *config.Paths, configFile *string) *cobra.Command {
	pairing := &cobra.Command{Use: "pairing", Short: "Manage local pairing identity"}
	var platform string
	reset := &cobra.Command{Use: "reset", Short: "Clear one platform identity and automatic session pins", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !supportedIMPlatform(platform) {
			return errors.New("--platform must name a supported IM platform")
		}
		store, locks, err := loadLockedConfiguration(*configFile, paths)
		if err != nil {
			return err
		}
		defer locks.Close()
		cfg := store.Snapshot()
		if cfg.IMConfigByType(platform) == nil {
			return errors.New("platform is not configured")
		}
		terminal, err := openPairingTerminal()
		if err != nil {
			return errors.New("reset requires a controlling terminal")
		}
		defer terminal.Close()
		warning := "This clears the owner, chat bindings, and automatic session pins. Workspaces and session files are preserved."
		if _, err = fmt.Fprintf(terminal, "%s\nType %s to confirm: ", warning, platform); err != nil {
			return errors.New("could not display reset confirmation")
		}
		input, err := readPairingConfirmation(terminal)
		if err != nil {
			return err
		}
		if strings.TrimSpace(input) != platform {
			return errors.New("reset cancelled")
		}
		if err = resetPairing(context.Background(), store, paths, platform); err != nil {
			return err
		}
		_, err = fmt.Fprintln(terminal, "Pairing reset completed. Start the gateway with --pair to register an owner in a private chat.")
		return err
	}}
	reset.Flags().StringVar(&platform, "platform", "", "Platform to reset: wechat, feishu, telegram, discord, wecom, dingtalk, slack")
	pairing.AddCommand(reset)
	return pairing
}

func resetPairing(ctx context.Context, store *ConfigStore, paths *config.Paths, platform string) error {
	transfers := newSessionTransferStore(paths.StateDir)
	if err := transfers.Load(); err != nil {
		return errors.New("cannot reset while transfer state cannot be read")
	}
	for key := range transfers.entries {
		_, im, _ := parseProcessKey(key)
		if im == "" || im == platform {
			return errors.New("resolve platform session transfers before resetting pairing")
		}
	}
	pins := newSessionPinStore(paths.StateDir)
	if file := store.Snapshot().SessionPinsFile; file != "" {
		pins.path = filepath.Clean(file)
	}
	if raw, err := os.ReadFile(pins.path); err == nil && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("cannot reset an invalid null session pin file")
	}
	if err := pins.Load(); err != nil {
		return errors.New("cannot reset while session pins cannot be read")
	}
	if err := pins.ClearPlatform(platform); err != nil {
		return fmt.Errorf("clear platform session pins: %w", err)
	}
	outcome, err := store.Commit(ctx, ConfigMutation{Apply: func(doc *yaml.Node, _ *config.Config) error {
		ims, e := independentMapping(doc.Content[0], "ims")
		if e != nil {
			return e
		}
		node, e := independentIMMapping(ims, platform)
		if e != nil {
			return e
		}
		for _, field := range []string{"owner", "chat_bindings"} {
			removeMappingValue(node, field)
		}
		return nil
	}})
	if err != nil {
		if outcome.Phase >= Replaced {
			return errors.New("reset outcome is unknown; inspect the local configuration before restarting")
		}
		return errors.New("reset was not completed; identity is unchanged but automatic session pins may have been cleared")
	}
	return nil
}

func readConfirmationFromFile(file *os.File) (string, error) {
	return bufio.NewReader(file).ReadString('\n')
}
