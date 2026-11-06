//go:build windows

package config

import (
	"errors"
	"io/fs"
	"unsafe"

	"golang.org/x/sys/windows"
)

const readAccessMask = windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL

// inspectPermissions reads the file's ACL because POSIX bits say nothing on
// Windows. Deny entries are deliberately ignored: this can only over-report, and
// a warning is safer than wrongly calling a file private.
func inspectPermissions(path string) (bool, error) {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return false, fs.ErrNotExist
		}
		return false, err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			return true, nil // a NULL DACL grants everyone full access
		}
		return false, err
	}
	if dacl == nil {
		return true, nil
	}
	trusted, err := trustedSIDs(descriptor)
	if err != nil {
		return false, err
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 ||
			ace.Mask&readAccessMask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !containsSID(trusted, sid) {
			return true, nil
		}
	}
	return false, nil
}

func trustedSIDs(descriptor *windows.SECURITY_DESCRIPTOR) ([]*windows.SID, error) {
	var trusted []*windows.SID
	owner, _, err := descriptor.Owner()
	if err != nil {
		return nil, err
	}
	if owner != nil {
		trusted = append(trusted, owner)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	trusted = append(trusted, user.User.Sid)
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid, windows.WinCreatorOwnerSid,
	} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return nil, err
		}
		trusted = append(trusted, sid)
	}
	return trusted, nil
}

func containsSID(sids []*windows.SID, target *windows.SID) bool {
	for _, sid := range sids {
		if sid.Equals(target) {
			return true
		}
	}
	return false
}
