//go:build windows

package auth

import (
	"os/user"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var setEntriesInACL = windows.NewLazySystemDLL("advapi32.dll").NewProc("SetEntriesInAclW")

// secureKeyFile replaces the inherited Windows ACL with an owner-only DACL.
func secureKeyFile(path string) error {
	current, err := user.Current()
	if err != nil {
		return err
	}
	sid, _, _, err := windows.LookupSID("", current.Username)
	if err != nil {
		return err
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_READ | windows.GENERIC_WRITE | windows.DELETE | windows.READ_CONTROL,
		AccessMode:        windows.SET_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	var dacl *windows.ACL
	r1, _, _ := setEntriesInACL.Call(
		uintptr(1),
		uintptr(unsafe.Pointer(&entry)),
		0,
		uintptr(unsafe.Pointer(&dacl)),
	)
	if r1 != 0 {
		return syscall.Errno(r1)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(dacl)))
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}
