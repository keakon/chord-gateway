package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/keakon/chord-gateway/config"
)

var errInstanceRunning = errors.New("another gateway instance holds a required resource")

func canonicalResource(path string) (string, error) {
	p, err := filepath.Abs(config.Expand(path))
	if err != nil {
		return "", err
	}
	p = filepath.Clean(p)
	var suffix []string
	for {
		resolved, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", e
		}
		suffix = append(suffix, filepath.Base(p))
		p = parent
	}
}

func instanceResources(path string, cfg *config.Config, paths *config.Paths) ([]string, error) {
	pins := cfg.SessionPinsFile
	if pins == "" {
		pins = filepath.Join(paths.StateDir, "session-pins.json")
	}
	raw := []string{path, paths.StateDir, pins}
	if im := cfg.IMConfigByType("wechat"); im != nil && im.Wechat.TokenPath != "" {
		raw = append(raw, im.Wechat.TokenPath)
	}
	unique := map[string]bool{}
	var result []string
	for _, p := range raw {
		r, err := canonicalResource(p)
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

type instanceLocks struct{ files []*os.File }

func (l *instanceLocks) Close() {
	for i := len(l.files) - 1; i >= 0; i-- {
		releaseInstanceLock(l.files[i])
		_ = l.files[i].Close()
	}
	l.files = nil
}
func acquireInstanceLocks(resources []string) (*instanceLocks, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(cache, "chord-gateway", "instance-locks")
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err = os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	locks := &instanceLocks{}
	success := false
	defer func() {
		if !success {
			locks.Close()
		}
	}()
	for _, resource := range resources {
		name := fmt.Sprintf("%x.lock", sha256.Sum256([]byte(resource)))
		f, e := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o600)
		if e != nil {
			return nil, e
		}
		if e = f.Chmod(0o600); e != nil {
			f.Close()
			return nil, e
		}
		if e = takeInstanceLock(f); e != nil {
			f.Close()
			return nil, e
		}
		locks.files = append(locks.files, f)
	}
	success = true
	return locks, nil
}

// loadLockedConfiguration repeats read-only discovery once if a concurrent
// local edit changed the resource set before all locks were acquired.
func loadLockedConfiguration(path string, paths *config.Paths) (*ConfigStore, *instanceLocks, error) {
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
		locks, lockErr := acquireInstanceLocks(resources)
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
