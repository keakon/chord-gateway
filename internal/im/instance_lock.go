package im

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrInstanceRunning = errors.New("another gateway instance holds a required resource")

type InstanceLocks struct{ files []*os.File }

// InstanceLockCacheDir is replaced by tests before starting workers; production
// always uses the per-user directory, shared across configurations and state
// directories.
var InstanceLockCacheDir = os.UserCacheDir

func (l *InstanceLocks) Close() {
	for i := len(l.files) - 1; i >= 0; i-- {
		releaseInstanceLock(l.files[i])
		_ = l.files[i].Close()
	}
	l.files = nil
}
func AcquireInstanceLocks(resources []string) (*InstanceLocks, error) {
	cache, err := InstanceLockCacheDir()
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
	locks := &InstanceLocks{}
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
