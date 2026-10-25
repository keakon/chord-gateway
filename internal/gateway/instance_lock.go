package gateway

import (
	"errors"
	"path/filepath"
	"sort"

	"github.com/keakon/chord-gateway/config"
	"github.com/keakon/chord-gateway/internal/im"
)

func instanceResources(path string, cfg *config.Config, paths *config.Paths) ([]string, error) {
	pins := cfg.SessionPinsFile
	if pins == "" {
		pins = filepath.Join(paths.StateDir, "session-pins.json")
	}
	raw := []string{path, paths.StateDir, pins}
	if imCfg := cfg.IMConfigByType("wechat"); imCfg != nil && imCfg.Wechat.TokenPath != "" {
		raw = append(raw, imCfg.Wechat.TokenPath)
	}
	unique := map[string]bool{}
	var result []string
	for _, p := range raw {
		r, err := config.CanonicalResource(p)
		if err != nil {
			return nil, err
		}
		if !unique[r] {
			unique[r] = true
			result = append(result, r)
		}
	}
	sort.Strings(result)
	return result, nil
}

// loadLockedConfiguration repeats read-only discovery once if a concurrent
// local edit changed the resource set before all locks were acquired.
func loadLockedConfiguration(path string, paths *config.Paths) (*ConfigStore, *im.InstanceLocks, error) {
	for attempt := 0; attempt < 2; attempt++ {
		cfg, err := config.Load(path)
		if err != nil {
			return nil, nil, err
		}
		if err = cfg.Validate(); err != nil {
			return nil, nil, err
		}
		resources, err := instanceResources(path, cfg, paths)
		if err != nil {
			return nil, nil, err
		}
		locks, lockErr := im.AcquireInstanceLocks(resources)
		if lockErr != nil {
			return nil, nil, lockErr
		}
		store, err := NewConfigStore(path)
		if err != nil {
			if locks != nil {
				locks.Close()
			}
			return nil, nil, err
		}
		next, err := instanceResources(path, store.Snapshot(), paths)
		if err != nil {
			if locks != nil {
				locks.Close()
			}
			return nil, nil, err
		}
		same := len(resources) == len(next)
		if same {
			for i := range resources {
				same = same && resources[i] == next[i]
			}
		}
		if !same {
			if locks != nil {
				locks.Close()
			}
			continue
		}
		return store, locks, nil
	}
	return nil, nil, errors.New("configuration resources changed repeatedly during startup")
}
