//go:build linux

package secrets

// Hermetic constructors for the Linux Secret Service D-Bus wire framing.
//
// The Linux backend speaks org.freedesktop.secrets over the session bus in
// pure Go (github.com/godbus/dbus/v5 — no libsecret, no cgo). Everything
// about those method calls that can be decided without a daemon — attribute
// maps, the Secret struct's field order, the replace flag — is decided HERE,
// so the OS-boundary logic is unit-testable at 100% statement coverage
// headless, with no session bus. The wrappers (backend_linux.go,
// store_linux.go) only dial, call, and unwrap.
//
// This file is linux-tagged, like the backend it serves: godbus's own
// darwin/windows platform files do not compile under the Go 1.27 toolchain
// (v5.2.3 conn_darwin.go returns a bare string where (string, error) is
// wanted — a pre-existing dependency limitation, not Pulse code), so any
// file importing godbus must stay out of those builds. The darwin argv seam
// (cmdbuilder.go) and the windows encoding seam (credencode.go) use stdlib
// only and stay untagged, running on every host.
//
// Field order is the wire contract: D-Bus structs serialize positionally, so
// dbusSecret's order (session, parameters, value, content_type) mirrors the
// specification's Secret struct exactly, and the signature test pins the
// resulting "(oayays)" encoding.
//
// content_type is a STRING, not a byte array: the daemon rejects
// (oayayay) with "does not match expected type (a{sv}(oayays)b)" — found
// live against gnome-keyring (Wave 6.5), not from the spec text alone.

import (
	"github.com/godbus/dbus/v5"
)

// D-Bus constants shared by the read and write paths. The platform-tagged
// files reference these rather than restating them, so the two cannot drift
// into addressing different services.
const (
	dbusSecretServiceName  = "org.freedesktop.secrets"
	dbusSecretServicePath  = "/org/freedesktop/secrets"
	dbusSecretServiceIface = "org.freedesktop.Secret.Service"
	dbusSecretItemIface    = "org.freedesktop.Secret.Item"
	dbusCollectionIface    = "org.freedesktop.Secret.Collection"
	dbusPropertiesIface    = "org.freedesktop.DBus.Properties"
)

// CreateItem property keys (org.freedesktop.Secret.Collection.CreateItem
// takes a{sv}, not the a{ss} attribute map SearchItems takes).
const (
	dbusItemLabelKey = "org.freedesktop.Secret.Item.Label"
	dbusItemAttrsKey = "org.freedesktop.Secret.Item.Attributes"
)

// defaultAlias is the collection alias both paths resolve: the login
// keyring. gnome-keyring does NOT publish a DefaultCollection property
// (reading it fails live with "No such property"), so both paths use
// ReadAlias — proven against the daemon in Wave 6.5.

// dbusSecret mirrors the Secret Service Secret struct:
//
//	(session o, parameters ay, value ay, content_type s)
//
// It is encoded as a Go struct in the same field order, which is the form
// godbus expects for a struct-typed argument.
type dbusSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// dbusSecretSignature is the wire encoding the struct must produce. Pinned by
// test via dbus.SignatureOf AND proven live: the daemon rejects anything
// else at the CreateItem call. Any field reorder, add, or retype changes
// the signature and fails the gate without needing a daemon to reject it.
const dbusSecretSignature = "(oayays)"

// searchAttrs builds the attribute map identifying one Pulse item: the read
// path searches for exactly what the write path stores, constructed by the
// same function, so the two cannot disagree on a key name.
func searchAttrs(service, account string) map[string]string {
	return map[string]string{
		"service": service,
		"account": account,
	}
}

// newDBusSecret builds the Secret argument for a store call: the plain
// (unencrypted) session, no algorithm parameters, the value bytes, and an
// explicit text/plain content type.
func newDBusSecret(session dbus.ObjectPath, value string) dbusSecret {
	return dbusSecret{
		Session:     session,
		Parameters:  []byte{},
		Value:       []byte(value),
		ContentType: "text/plain",
	}
}

// createItemProps builds the CreateItem properties map (a{sv}): the item
// label plus the attribute map under its spec key. The attributes use
// searchAttrs — the same constructor the read path searches with — so a
// stored item is always searchable. (The write previously stored under one
// map while the read searched another; MEMORY.md ADL-0041.)
func createItemProps(service, account string) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		dbusItemLabelKey: dbus.MakeVariant("pulse: " + account),
		dbusItemAttrsKey: dbus.MakeVariant(searchAttrs(service, account)),
	}
}

// createItemParams builds the CreateItem argument triple: properties,
// secret, and replace=true so `vault init` on a machine that already holds
// a key updates the item instead of failing on a duplicate.
func createItemParams(session dbus.ObjectPath, service, account, value string) (map[string]dbus.Variant, dbusSecret, bool) {
	return createItemProps(service, account), newDBusSecret(session, value), true
}
