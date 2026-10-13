package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"strings"

	"github.com/keakon/chord-gateway/config"
)

func pinKeyForBinding(cfg *config.Config, key string) string {
	_, platform, _ := parseProcessKey(key)
	if cfg == nil || cfg.IMConfigByType(platform) == nil || platform == "console" {
		return key
	}
	owner := cfg.IMConfigByType(platform).OwnerIdentity()
	if owner == nil {
		return "paired:unowned:" + key
	}
	scope, _ := json.Marshal([]string{platform, owner.AccountID, owner.UserID})
	return "paired:v1:" + base64.RawURLEncoding.EncodeToString(scope) + ":" + key
}
func (p *ChordProcess) sessionPinKey() string {
	if p.pinKey != "" {
		return p.pinKey
	}
	return p.key
}

func pinPlatform(key string) (string, error) {
	if strings.HasPrefix(key, "paired:") {
		rest, ok := strings.CutPrefix(key, "paired:v1:")
		if !ok {
			return "", errors.New("invalid scoped session pin")
		}
		encoded, process, ok := strings.Cut(rest, ":")
		if !ok {
			return "", errors.New("invalid scoped session pin")
		}
		data, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return "", errors.New("invalid session pin scope")
		}
		var scope []string
		if json.Unmarshal(data, &scope) != nil || len(scope) != 3 || scope[1] == "" || scope[2] == "" {
			return "", errors.New("invalid session pin scope")
		}
		_, platform, _ := parseProcessKey(process)
		if scope[0] != platform {
			return "", errors.New("inconsistent session pin scope")
		}
		key = process
	}
	workspace, platform, chat := parseProcessKey(key)
	if workspace == "" || chat == "" || (platform != "feishu" && platform != "wechat" && platform != "console") {
		return "", errors.New("unrecognized session pin key")
	}
	return platform, nil
}

// ClearPlatform persists the entire removal before publishing it in memory.
// Call Load and check its error before using this method for local reset.
func (s *sessionPinStore) ClearPlatform(platform string) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	next := maps.Clone(s.pins)
	s.mu.Unlock()
	for key := range next {
		p, err := pinPlatform(key)
		if err != nil {
			return err
		}
		if p == platform {
			delete(next, key)
		}
	}
	if err := s.savePins(next); err != nil {
		return err
	}
	s.mu.Lock()
	s.pins = next
	s.mu.Unlock()
	return nil
}
