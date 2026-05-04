package main

import (
	"errors"
	"fmt"
	"sync"

	"github.com/keakon/golog/log"

	"github.com/keakon/chord-gateway/config"
)

// MultiAdapter runs multiple IM adapters in parallel.
type MultiAdapter struct {
	adapters []IMAdapter
	router   *NotificationRouter
}

// NewMultiAdapter creates a MultiAdapter from the active IM configs.
func NewMultiAdapter(cfg *config.Config, paths *config.Paths, router *NotificationRouter) (*MultiAdapter, error) {
	activeIMs := cfg.ActiveIMs()
	if len(activeIMs) == 0 {
		return nil, fmt.Errorf("no IM adapters configured")
	}

	m := &MultiAdapter{router: router}
	for i := range activeIMs {
		a, err := newAdapterFromConfig(activeIMs[i], cfg, paths, router)
		if err != nil {
			return nil, fmt.Errorf("create %s adapter: %w", activeIMs[i].Type(), err)
		}
		m.adapters = append(m.adapters, a)
	}
	return m, nil
}

// Connect starts all adapters in parallel and blocks until all exit.
func (m *MultiAdapter) Connect() error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, a := range m.adapters {
		wg.Add(1)
		go func(a IMAdapter) {
			defer wg.Done()
			log.Infof("starting IM adapter type=%v", a.Type())
			if err := a.Connect(); err != nil {
				log.Errorf("IM adapter exited with error type=%v error=%v", a.Type(), err)
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", a.Type(), err))
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// SendText sends a text message to the given chatID via the appropriate adapter.
func (m *MultiAdapter) SendText(chatID, text string) error {
	if m.router != nil {
		if adapterType := m.router.adapterTypeForChatID(chatID); adapterType != "" {
			return m.SendTextVia(adapterType, chatID, text)
		}
	}
	var errs []error
	for _, a := range m.adapters {
		if err := a.SendText(chatID, text); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Type(), err))
			continue
		}
		return nil
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return fmt.Errorf("no IM adapters configured")
}

// SendTextVia sends a text message through a specific adapter.
func (m *MultiAdapter) SendTextVia(adapterType, chatID, text string) error {
	adapter := m.FindAdapterByType(adapterType)
	if adapter == nil {
		return fmt.Errorf("adapter %q not found", adapterType)
	}
	return adapter.SendText(chatID, text)
}

// Disconnect shuts down all adapters.
func (m *MultiAdapter) Disconnect() {
	for _, a := range m.adapters {
		a.Disconnect()
	}
}

// Type returns "multi".
func (m *MultiAdapter) Type() string { return "multi" }

// StartLogin returns ErrLoginNotSupported.
// Login routing for the active adapter is handled by NotificationRouter via
// findAdapterByType. This method exists only to satisfy the IMAdapter interface
// for the multi adapter wrapper.
func (m *MultiAdapter) StartLogin() (string, error) {
	return "", ErrLoginNotSupported
}

// FindAdapterByType returns the adapter matching the given type, or nil.
func (m *MultiAdapter) FindAdapterByType(adapterType string) IMAdapter {
	adapterType = config.NormalizeIMType(adapterType)
	for _, a := range m.adapters {
		if a.Type() == adapterType {
			return a
		}
	}
	return nil
}

// BroadcastTextExcept sends a text message through all adapters EXCEPT
// the one of the given type. Used for cross-IM notifications.
func (m *MultiAdapter) BroadcastTextExcept(excludeType string, chatIDs map[string]string, text string) {
	excludeType = config.NormalizeIMType(excludeType)
	for _, a := range m.adapters {
		if a.Type() == excludeType {
			continue
		}
		chatID := chatIDs[a.Type()]
		if chatID == "" && m.router != nil {
			chatID = m.router.chatIDForAdapter(a.Type())
		}
		if chatID == "" {
			log.Debugf("no chatID for adapter, skipping broadcast type=%v", a.Type())
			continue
		}
		if err := a.SendText(chatID, text); err != nil {
			log.Errorf("broadcast send failed type=%v chatID=%v error=%v", a.Type(), chatID, err)
		}
	}
}
