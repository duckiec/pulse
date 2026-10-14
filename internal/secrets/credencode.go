package secrets

// Hermetic constructors for the Windows Credential Manager value encoding.
//
// The Windows backend calls advapi32's CredRead/CredWrite through the pure-Go
// golang.org/x/sys/windows bindings — a syscall, not cgo — but x/sys/windows
// only compiles on Windows, so the struct-filling code in the
// platform-tagged files cannot run in CI. Everything about the credential
// that is pure data — the target-name namespacing and the UTF-16 blob
// encoding with its byte-size arithmetic — is decided HERE, in
// platform-independent code, so the OS-boundary logic is unit-testable at
// 100% statement coverage on any host. The tagged wrapper (store_windows.go)
// only places the encoded units into CREDENTIALW and issues the syscall.
//
// The encoding contract, in one place: the blob is UTF-16 with a terminating
// NUL (mirroring windows.UTF16FromString), CredentialBlobSize counts BYTES
// not runes, and an embedded NUL in the value is rejected rather than
// truncated — a truncated secret would authenticate as the wrong secret.

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// credTarget builds the Credential Manager target name for a key. The
// "pulse/" prefix namespaces Pulse's entries inside the user's credential
// store, so a read looks up exactly what the write stored.
func credTarget(key string) string { return ServiceName + "/" + key }

// encodeCredBlob encodes a credential value for CREDENTIALW: NUL-terminated
// UTF-16 units plus the blob size in bytes. The size is len(units)*2 —
// bytes, not UTF-16 code units and not runes — because that is what
// CredentialBlobSize means, and the read side divides by two to recover the
// units.
func encodeCredBlob(value string) (units []uint16, sizeBytes uint32, err error) {
	if strings.ContainsRune(value, 0) {
		// windows.UTF16FromString rejects NUL the same way; accepting it
		// here would write a secret the read side decodes differently.
		return nil, 0, fmt.Errorf("secrets: credential value contains a NUL byte")
	}
	units = append(utf16.Encode([]rune(value)), 0)
	return units, uint32(len(units) * 2), nil
}

// decodeCredBlob reverses encodeCredBlob: even-length byte blobs decode to
// UTF-16 units with trailing NULs trimmed (advapi32 does not guarantee the
// terminator). It mirrors the read side's decoding so the hermetic tests
// prove the round trip without Credential Manager.
func decodeCredBlob(blob []byte) (string, error) {
	if len(blob) == 0 || len(blob)%2 != 0 {
		// A blob whose length is not a whole number of UTF-16 code units is
		// malformed; guessing at the trailing byte could yield a wrong
		// secret, so fail closed exactly as the read side does.
		return "", fmt.Errorf("secrets: credential blob has malformed length %d", len(blob))
	}
	units := make([]uint16, 0, len(blob)/2)
	for i := 0; i < len(blob); i += 2 {
		units = append(units, uint16(blob[i])|uint16(blob[i+1])<<8)
	}
	for len(units) > 0 && units[len(units)-1] == 0 {
		units = units[:len(units)-1]
	}
	if len(units) == 0 {
		return "", fmt.Errorf("secrets: credential blob decoded to empty")
	}
	return string(utf16.Decode(units)), nil
}
