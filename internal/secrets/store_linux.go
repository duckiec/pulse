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
// This path is proven live against gnome-keyring under
// scripts/ci/run-with-keyring.sh (Wave 6.5, internal/secrets/linux_live_test.go
// with the integration tag); the hermetic constructor tests pin the framing
// headless.

// dbusSecret lives in dbusframe.go, shared with the hermetic tests that pin
// its wire signature.

type linuxStore struct{ backend *dbusBackend }

func newPlatformStore() SecretStore { return &linuxStore{backend: &dbusBackend{}} }

func (s *linuxStore) Backend() string { return "secret-service-dbus" }

// Available reports whether the Secret Service is reachable.
func (s *linuxStore) Available() error {
	return s.backend.Available()
}

// Store implements SecretStore.
//
// CreateItem lives on the COLLECTION object, not the service: calling it on
// /org/freedesktop/secrets fails live with "no such method". The
// collection is the same default alias the read path resolves, so the two
// cannot drift apart (Wave 6.5).
func (s *linuxStore) Store(key, value string) error {
	if err := s.Available(); err != nil {
		return err
	}
	conn, err := s.backend.connect()
	if err != nil {
		return err
	}
	collection, err := s.backend.defaultCollection()
	if err != nil {
		return err
	}
	session, err := s.backend.openPlainSession(conn)
	if err != nil {
		return err
	}

	// Properties, secret struct, and the replace flag come from the shared
	// constructor: the write stores under exactly the attributes the read
	// searches for. (Previously this map was {ServiceName: key} while the
	// read searched {service, account} — items were written that no read
	// could ever find. MEMORY.md ADL-0041.)
	props, secret, replace := createItemParams(session, ServiceName, key, value)

	var item, prompt dbus.ObjectPath
	if err := conn.Object(dbusSecretServiceName, collection).Call(
		dbusCollectionIface+".CreateItem", 0, props, secret, replace).Store(&item, &prompt); err != nil {
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
