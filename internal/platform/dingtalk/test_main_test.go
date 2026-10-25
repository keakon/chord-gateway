package dingtalk

import (
	"fmt"
	"os"
	"testing"

	"github.com/keakon/chord-gateway/internal/im"
)

// One root per test process isolates machine-wide locks, mirroring the root
// package's TestMain so identity tests never touch the developer's own cache.
var testProcessRoot string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "chord-gateway-dingtalk-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testProcessRoot = root
	im.InstanceLockCacheDir = func() (string, error) { return root, nil }
	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
