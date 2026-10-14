//go:build linux

package secrets

import (
	"fmt"

	"github.com/godbus/dbus/v5"
)

// The Linux backend talks the Secret Service D-Bus API directly in pure Go. No
// libsecret, no cgo — the wire protocol is spoken by github.com/godbus/dbus/v5.
//
// Protocol shape (org.freedesktop.secrets):
//
//	Service        org.freedesktop.secrets
//	Object path    /org/freedesktop/secrets
//	Interface      org.freedesktop.Secret.Service
//	OpenSession    (s algorithm) -> (v session_path, v result)
//	SearchItems    (a{ss} attributes) -> (ao unlocked[], ao locked[])
//	ReadAlias      (s name) -> (o collection)
//	Unlock         (ao objects, s password) -> (ao unlocked, v prompt)
//
// An item is opened as an org.freedesktop.Secret.Item and its secret read as
// org.freedesktop.Secret.Item.GetSecret(session) -> (o session, ay password,
// ay content_type). An encrypted item must be unlocked first, which for a
// keyring locked at login requires a user prompt — a headless session has
// nobody to prompt, so that path reports BLOCKED rather than failing silently.

const (
	// DefaultCollection is the login keyring's default alias.
	DefaultCollection = "default"
)

// The D-Bus service, path, and interface names live in dbusframe.go, shared
// with the write path so the two cannot drift into addressing different
// services.

// dbusBackend is the Linux Secret Service backend.
type dbusBackend struct {
	conn    *dbus.Conn
	connErr error
	session dbus.ObjectPath
}

// newPlatformBackend is the linux build of NewBackend.
func newPlatformBackend() SecretResolver { return &dbusBackend{} }

// Backend implements SecretResolver.
func (b *dbusBackend) Backend() string { return "secret-service-dbus" }

// Available probes the Secret Service over the session bus. Every failure mode
// here is a legitimate BLOCKED condition in a headless environment, and each is
// reported with the reason rather than swallowed.
//
// The probe resolves the default alias: it proves the daemon is on the bus,
// speaking the expected API, AND holding an unlocked login collection.
// (The DefaultCollection property is NOT probed: gnome-keyring does not
// publish it — reading it fails live with "No such property". Wave 6.5.)
func (b *dbusBackend) Available() error {
	if _, err := b.defaultCollection(); err != nil {
		return err
	}
	return nil
}

// defaultCollection resolves the login collection both paths operate on.
// A "/" result means "no default alias": nothing to read from and
// nowhere to store to, reported as unavailable rather than dialled further.
func (b *dbusBackend) defaultCollection() (dbus.ObjectPath, error) {
	conn, err := b.connect()
	if err != nil {
		return "", err
	}
	svc := conn.Object(dbusSecretServiceName, dbus.ObjectPath(dbusSecretServicePath))
	var aliasPath dbus.ObjectPath
	if err := svc.Call(dbusSecretServiceIface+".ReadAlias", 0, DefaultCollection).Store(&aliasPath); err != nil {
		return "", fmt.Errorf("secrets: read default collection: %w", err)
	}
	if aliasPath == "" || aliasPath == "/" {
		return "", fmt.Errorf("secrets: secret service has no default collection (keyring locked or absent)")
	}
	return aliasPath, nil
}

// openPlainSession negotiates an unencrypted session with the service.
// godbus splits inputs (Call args) from outputs (Store targets): passing
// output pointers as Call args encodes them onto the wire and fails client-
// side — found live in Wave 6.5, where OpenSession never succeeded.
func (b *dbusBackend) openPlainSession(conn *dbus.Conn) (dbus.ObjectPath, error) {
	svc := conn.Object(dbusSecretServiceName, dbus.ObjectPath(dbusSecretServicePath))
	var output dbus.Variant
	var sessionPath dbus.ObjectPath
	if err := svc.Call(dbusSecretServiceIface+".OpenSession",
		0, "plain", dbus.MakeVariant("")).Store(&output, &sessionPath); err != nil {
		return "", fmt.Errorf("secrets: open session: %w", err)
	}
	if sessionPath == "" || sessionPath == "/" {
		return "", fmt.Errorf("secrets: open session returned no session path")
	}
	return sessionPath, nil
}

// connect returns the session bus connection, dialing lazily and caching it.
func (b *dbusBackend) connect() (*dbus.Conn, error) {
	if b.conn != nil {
		return b.conn, nil
	}
	if b.connErr != nil {
		return nil, b.connErr
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		b.connErr = fmt.Errorf("secrets: no D-Bus session bus (headless or no dbus-daemon): %w", err)
		return nil, b.connErr
	}
	b.conn = conn
	return conn, nil
}

// Resolve reads one secret from the keychain by account name.
func (b *dbusBackend) Resolve(key string) (string, error) {
	conn, err := b.connect()
	if err != nil {
		return "", err
	}
	svc := conn.Object(dbusSecretServiceName, dbus.ObjectPath(dbusSecretServicePath))

	// Resolve the collection alias (the login keyring).
	if _, err := b.defaultCollection(); err != nil {
		return "", err
	}

	// Open an algorithm-agnostic session. A plain session's output
	// variant is legitimately empty — the session PATH is the result.
	sessionPath, err := b.openPlainSession(conn)
	if err != nil {
		return "", err
	}
	b.session = sessionPath

	// Search the collection for an unlocked item with our account label, using
	// the same constructor the write path stores under: the two agree by
	// construction, not by convention (MEMORY.md ADL-0041).
	attrs := searchAttrs(ServiceName, key)
	var unlocked, locked []dbus.ObjectPath
	if err := svc.Call(dbusSecretServiceIface+".SearchItems", 0, attrs).Store(&unlocked, &locked); err != nil {
		return "", fmt.Errorf("secrets: search keyring: %w", err)
	}
	switch {
	case len(unlocked) > 0:
		return b.readItem(conn, unlocked[0], sessionPath)
	case len(locked) > 0:
		// Unlocking needs the user's login password; a headless process has no
		// one to ask. Report BLOCKED, never return a placeholder.
		return "", fmt.Errorf("secrets: %q is locked and unlocking requires an interactive prompt", key)
	default:
		return "", fmt.Errorf("secrets: %q %w", key, ErrNotFound)
	}
}

// readItem pulls the secret bytes out of an item.
//
// GetSecret returns ONE struct argument (oayays), not three separate
// values: storing into three targets fails with a length mismatch.
// (Wave 6.5, live against gnome-keyring.)
func (b *dbusBackend) readItem(conn *dbus.Conn, item, session dbus.ObjectPath) (string, error) {
	var got dbusSecret
	call := conn.Object(dbusSecretServiceName, item).Call(dbusSecretItemIface+".GetSecret", 0, session)
	if err := call.Store(&got); err != nil {
		return "", fmt.Errorf("secrets: read secret: %w", err)
	}
	if len(got.Value) == 0 {
		return "", fmt.Errorf("secrets: keychain returned an empty secret")
	}
	return string(got.Value), nil
}
