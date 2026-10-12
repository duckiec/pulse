//go:build windows

package secrets

import (
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows backend calls advapi32's CredRead through the pure-Go
// golang.org/x/sys/windows bindings. That is a syscall, not cgo, so it satisfies
// the CGO_ENABLED=0 mandate (ARCHITECTURE.md §6.2).
//
//	CredReadW(targetName, type=CRED_TYPE_GENERIC, flags=0, &cred)
//	CredFree(cred)
//
// The blob at CredentialBlob is UTF-16 and, per the docs, may or may not carry a
// terminating NUL — both are handled below.

// winCred is the layout of CREDENTIALW as advapi32 fills it. Field order and
// widths must match the Win32 struct exactly.
type winCred struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// windowsBackend is the Windows Credential Manager backend.
type windowsBackend struct{}

// newPlatformBackend is the windows build of NewBackend.
func newPlatformBackend() SecretResolver { return windowsBackend{} }

// Backend implements SecretResolver.
func (windowsBackend) Backend() string { return "windows-credential-manager" }

// Available reports whether Credential Manager is reachable. advapi32 is present
// on every supported Windows build, so this is a library-load check.
func (windowsBackend) Available() error {
	if _, err := windows.LoadDLL("advapi32.dll"); err != nil {
		return fmt.Errorf("secrets: advapi32.dll unavailable: %w", err)
	}
	return nil
}

// Resolve reads one generic credential by target name.
func (w windowsBackend) Resolve(key string) (string, error) {
	if err := w.Available(); err != nil {
		return "", err
	}
	advapi := syscall.NewLazyDLL("advapi32.dll")
	credRead := advapi.NewProc("CredReadW")
	credFree := advapi.NewProc("CredFree")
	if err := credRead.Find(); err != nil {
		return "", fmt.Errorf("secrets: CredReadW unavailable: %w", err)
	}

	target, err := windows.UTF16PtrFromString(credTarget(key))
	if err != nil {
		return "", fmt.Errorf("secrets: invalid credential name %q: %w", key, err)
	}
	var cred *winCred
	const credTypeGeneric = 1
	r, _, callErr := credRead.Call(
		uintptr(unsafe.Pointer(target)),
		uintptr(credTypeGeneric),
		0,
		uintptr(unsafe.Pointer(&cred)),
	)
	if r == 0 {
		// ERROR_NOT_FOUND (1168) is a plain miss, not a broken backend.
		if errno, ok := callErr.(windows.Errno); ok && errno == windows.ERROR_NOT_FOUND {
			return "", fmt.Errorf("secrets: %q not found in Credential Manager", key)
		}
		return "", fmt.Errorf("secrets: CredRead failed for %q: %w", key, callErr)
	}
	defer credFree.Call(uintptr(unsafe.Pointer(cred)))

	if cred.CredentialBlobSize == 0 || cred.CredentialBlob == nil {
		return "", fmt.Errorf("secrets: credential for %q is empty", key)
	}
	blob := unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize)
	if len(blob) == 0 || len(blob)%2 != 0 {
		// A blob whose length is not a whole number of UTF-16 code units is
		// malformed; guessing at the trailing byte could yield a wrong secret.
		return "", fmt.Errorf("secrets: credential for %q has a malformed blob length %d", key, len(blob))
	}
	units := unsafe.Slice((*uint16)(unsafe.Pointer(&blob[0])), len(blob)/2)
	// The blob is not guaranteed NUL-terminated; trim if one is present.
	for len(units) > 0 && units[len(units)-1] == 0 {
		units = units[:len(units)-1]
	}
	if len(units) == 0 {
		return "", fmt.Errorf("secrets: credential for %q decoded to empty", key)
	}
	return string(utf16.Decode(units)), nil
}

// credTarget builds the Credential Manager target name for a key. The
// "pulse/" prefix namespaces Pulse's entries inside the user's credential store.
func credTarget(key string) string { return ServiceName + "/" + key }
