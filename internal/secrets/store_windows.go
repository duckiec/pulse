//go:build windows

package secrets

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows write path calls advapi32's CredWriteW through the pure-Go
// golang.org/x/sys/windows bindings, mirroring the CredReadW route the read
// side already uses. A syscall is not cgo, so it satisfies the CGO_ENABLED=0
// mandate (ARCHITECTURE.md §6.2).
//
//	CredWriteW(cred, 0)
//
// The credential is built as CREDENTIALW with CRED_TYPE_GENERIC, and the
// credential blob is UTF-16 -- the same encoding the read side already decodes,
// so the two agree by construction rather than by convention.
type winStore struct{}

func newPlatformStore() SecretStore { return winStore{} }

func (winStore) Backend() string { return "windows-credman-advapi32" }

func (winStore) Available() error {
	// advapi32 is always present on Windows; the failure mode is a locked or
	// inaccessible credential manager, which only surfaces at write time.
	return nil
}

// credWriteW is the CREDENTIALW layout for a write. Field order and widths must
// match the Win32 struct exactly, which the read side's winCred already pins.
type credWriteW struct {
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

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
)

// Store implements SecretStore.
func (winStore) Store(key, value string) error {
	target, err := windows.UTF16PtrFromString(ServiceName)
	if err != nil {
		return fmt.Errorf("secrets: encode the keychain service name: %w", err)
	}
	account, err := windows.UTF16PtrFromString(key)
	if err != nil {
		return fmt.Errorf("secrets: encode the keychain account name: %w", err)
	}
	// The blob encoding (NUL-terminated UTF-16, size in bytes) comes from the
	// shared hermetic constructor, which the headless tests pin — including
	// the NUL-in-value rejection, which matches windows.UTF16FromString's.
	blob, blobSize, err := encodeCredBlob(value)
	if err != nil {
		return fmt.Errorf("secrets: encode the credential: %w", err)
	}

	cred := credWriteW{
		Type:               credTypeGeneric,
		TargetName:         account,
		CredentialBlobSize: blobSize,
		CredentialBlob:     (*byte)(unsafe.Pointer(&blob[0])),
		Persist:            credPersistLocalMachine,
		Comment:            target,
		UserName:           account,
	}

	advapi := windows.NewLazySystemDLL("advapi32.dll")
	credWrite := advapi.NewProc("CredWriteW")
	ret, _, err := credWrite.Call(uintptr(unsafe.Pointer(&cred)), 0)
	if ret == 0 {
		return fmt.Errorf("secrets: CredWriteW failed: %w", err)
	}
	return nil
}
