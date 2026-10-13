//go:build !darwin && !linux && !windows

package secrets

// Every other platform has no keychain write path. The error from Available
// names the reason, so `vault init` on such a host fails closed with something
// actionable rather than with a generic failure.
func newPlatformStore() SecretStore {
	return noStore{}
}
