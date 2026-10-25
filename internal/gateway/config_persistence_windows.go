//go:build windows

package gateway

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func protectConfigTemporary(f *os.File, path string, _ os.FileMode) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read configuration ACL: %w", err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read configuration DACL: %w", err)
	}
	if acl == nil {
		return fmt.Errorf("configuration must have a restricted DACL")
	}
	return windows.SetNamedSecurityInfo(f.Name(), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
func replaceConfigFile(temp, path string) (CommitOutcome, error) {
	from, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return CommitOutcome{BeforeReplace}, err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return CommitOutcome{BeforeReplace}, err
	}
	if err = windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return CommitOutcome{Replaced}, fmt.Errorf("replace configuration: %w", err)
	}
	return CommitOutcome{Durable}, nil
}
