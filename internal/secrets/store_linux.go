//go:build linux

package secrets

import (
	"fmt"

	"github.com/godbus/dbus/v5"
)

// The Linux write path speaks the Secret Service D-Bus API directly in pure Go,
// exactly as the read path does. No libsecret, no cgo.
//
//	Service   org.freedesktop.secrets
//	Path      /org/freedesktop/secrets
//	Method    org.freedesktop.Secret.Service.CreateItem
//	Args      (a{ss} attributes, v secret, b replace)
//	Returns   (s prompt)
//
// The `secret` argument is the Secret struct from the specification:
//
//	(session o, parameters ay, value ay, content_type ay)
//
// It is encoded here as a Go struct with the same field order, which is the form
// github.com/godbus/dbus/v5 expects for a struct-typed argument.
//
// This path is implemented but NOT TESTED in this repository's CI: the Linux
// environment it runs in has no Secret Service daemon. That is recorded as a
// BLOCKED gate rather than papered over (AGENTS.md §4.5).

// dbusSecret mirrors the Secret Service Secret struct.
type dbusSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType []byte
}

type linuxStore struct{ backend *dbusBackend }

func newPlatformStore() SecretStore { return &linuxStore{backend: &dbusBackend{}} }

func (s *linuxStore) Backend() string { return "secret-service-dbus" }

// Available reports whether the Secret Service is reachable.
func (s *linuxStore) Available() error {
	return s.backend.Available()
}

// Store implements SecretStore.
func (s *linuxStore) Store(key, value string) error {
	if err := s.Available(); err != nil {
		return err
	}
	conn, err := s.backend.connect()
	if err != nil {
		return err
	}
	svc := conn.Object(secretServiceName, dbus.ObjectPath(secretServicePath))

	// The plain (unencrypted) session is the service's own path, which is what
	// SearchItems uses for a read; CreateItem takes the same.
	var session dbus.ObjectPath
	if err := svc.Call("org.freedesktop.DBus.Properties.Get", 0,
		"org.freedesktop.Secret.Service", "DefaultCollection").Store(&session); err != nil {
		return fmt.Errorf("secrets: read default collection: %w", err)
	}

	// Attributes identify the item so the read path can find it again by
	// account name.
	attributes := map[string]string{
		ServiceName: key,
	}
	secret := dbusSecret{
		Session:     session,
		Parameters:  []byte{},
		Value:       []byte(value),
		ContentType: []byte("text/plain"),
	}

	// replace=true so `vault init` on a machine that already holds a key
	// updates the item instead of failing on a duplicate.
	var prompt string
	if err := svc.Call("org.freedesktop.Secret.Service.CreateItem", 0,
		attributes, secret, true).Store(&prompt); err != nil {
		return fmt.Errorf("secrets: CreateItem failed: %w", err)
	}
	if prompt != "" && prompt != "/" {
		// A non-empty prompt means the keyring needs unlocking. That is a
		// BLOCKED condition on a headless host, and it is reported rather than
		// silently ignored, because the value was not written.
		return fmt.Errorf("secrets: %w: the keychain requested a prompt at %s",
			ErrStoreUnavailable, prompt)
	}
	return nil
}
