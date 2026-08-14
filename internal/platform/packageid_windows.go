//go:build windows

package platform

import (
	"fmt"
	"os/user"
	"sync"
	"syscall"
	"unsafe"
)

// NOT VERIFIED ON REAL HARDWARE (M2). This file compiles for GOOS=windows
// in CI but has never executed: no Windows machine, and — the part that
// really matters — no MSIX-packaged client to be spawned by. See
// docs/status/windows.md.
//
// syscall.NewLazyDLL rather than golang.org/x/sys/windows: internal/platform
// is a zero-dependency foundation (docs/conventions.md#dependency-directions rule 4, depguard-enforced
// to $gostd only). The one call needed here is small enough that borrowing a
// module for it would be the more expensive choice.

// appModelErrorNoPackage is APPMODEL_ERROR_NO_PACKAGE: "the process has no
// package identity". It is the ONLY return code that means "not inside a
// container"; everything else — including unexpected error codes — is
// treated as "packaged", because the failure direction here is to keep the
// twin-path escape rather than to silently write into a shadow directory.
const appModelErrorNoPackage = 15700

var packageIdentityOnce = sync.OnceValue(probePackageIdentity)

// currentPackageIdentity reports whether this process runs inside an app
// container. The answer cannot change during a process lifetime, so it is
// computed once.
func currentPackageIdentity() PackageIdentity { return packageIdentityOnce() }

// probePackageIdentity calls GetCurrentPackageFamilyName.
//
// Failure direction: only APPMODEL_ERROR_NO_PACKAGE (15700) means "not
// packaged". Every other outcome — including a missing kernel32 export on
// an ancient build, or an error nobody anticipated — is reported as
// PACKAGED, because the consequence of guessing "not packaged" while inside
// a container is a silently redirected data directory, while the
// consequence of guessing "packaged" while outside one is a UNC probe that
// succeeds and a twin path that points at the very same directory.
func probePackageIdentity() PackageIdentity {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetCurrentPackageFamilyName")
	if err := proc.Find(); err != nil {
		// Pre-Windows 8 has no app model at all: no containers exist.
		return PackageIdentity{Packaged: false}
	}

	var length uint32
	rc, _, _ := proc.Call(uintptr(unsafe.Pointer(&length)), 0)
	if rc == appModelErrorNoPackage {
		return PackageIdentity{Packaged: false}
	}
	if length == 0 || length > 1<<16 {
		return PackageIdentity{Packaged: true}
	}

	buf := make([]uint16, length)
	rc, _, _ = proc.Call(uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buf[0])))
	if rc == appModelErrorNoPackage {
		return PackageIdentity{Packaged: false}
	}
	if rc != 0 { // ERROR_SUCCESS
		return PackageIdentity{Packaged: true}
	}
	return PackageIdentity{Packaged: true, Family: syscall.UTF16ToString(buf)}
}

// currentUserSID returns the current user's SID string. os/user fills Uid
// with the SID on Windows, which keeps this file free of a second syscall
// and of any dependency.
func currentUserSID() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if u.Uid == "" {
		return "", fmt.Errorf("empty SID for user %q", u.Username)
	}
	return u.Uid, nil
}
