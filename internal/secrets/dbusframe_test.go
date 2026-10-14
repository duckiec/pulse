//go:build linux

package secrets

// Linux-tagged half of the hermetic OS-boundary tests (Issue #9, Part 3).
//
// godbus's own darwin/windows platform files do not compile under Go 1.27,
// so anything importing it — the seam and these tests — stays linux-tagged,
// exactly like the backend it covers. Headless Linux CI (no session bus, no
// daemon) runs every assertion below: the constructors take plain values and
// return plain values, and the signature pin uses godbus's pure encoder.

import (
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

// TestDBusSecretWireSignature pins the Secret struct's D-Bus encoding:
// (session o, parameters ay, value ay, content_type s). Any field reorder,
// add, or retype changes the signature — failing here, without a daemon.
// (content_type was ay until Wave 6.5, when the live daemon rejected it:
// expected type (a{sv}(oayays)b). The pin now matches the daemon.)
func TestDBusSecretWireSignature(t *testing.T) {
	sig := dbus.SignatureOf(dbusSecret{
		Session:     "/org/freedesktop/secrets/session/s1",
		Parameters:  []byte{},
		Value:       []byte("v"),
		ContentType: "text/plain",
	}).String()
	if sig != dbusSecretSignature {
		t.Errorf("Secret wire signature = %q, want %q", sig, dbusSecretSignature)
	}
	if sig != "(oayays)" {
		t.Errorf("Secret wire signature = %q, want the daemon-proven (oayays)", sig)
	}
}

// TestDBusReadWriteAttributeParity proves the round trip BY CONSTRUCTION: the
// read's SearchItems map and the write's CreateItem map come from the same
// function, so a stored item is always searchable. (The write previously used
// {ServiceName: key} while the read searched {service, account} — items that
// no read could ever find. This test is the regression gate for that bug.)
func TestDBusReadWriteAttributeParity(t *testing.T) {
	readAttrs := searchAttrs("pulse", "vault:default")
	storedProps, secret, replace := createItemParams(
		"/org/freedesktop/secrets/session/s1", "pulse", "vault:default", "s3cr3t")
	// CreateItem takes a{sv}: the searchable attributes ride under the
	// spec's Attributes key. Extract and compare them, not the envelope.
	attrsVar, ok := storedProps[dbusItemAttrsKey]
	if !ok {
		t.Fatalf("CreateItem props lack %q: %v", dbusItemAttrsKey, storedProps)
	}
	storedAttrs, ok := attrsVar.Value().(map[string]string)
	if !ok {
		t.Fatalf("Attributes variant holds %T, want map[string]string", attrsVar.Value())
	}
	if _, ok := storedProps[dbusItemLabelKey]; !ok {
		t.Errorf("CreateItem props lack a label %q: %v", dbusItemLabelKey, storedProps)
	}
	if strings.Join(sortedKeys(readAttrs), ",") != strings.Join(sortedKeys(storedAttrs), ",") {
		t.Errorf("read attrs %v vs stored attrs %v: the round trip is broken", readAttrs, storedAttrs)
	}
	for k, v := range readAttrs {
		if storedAttrs[k] != v {
			t.Errorf("attribute %q: read %q vs stored %q", k, v, storedAttrs[k])
		}
	}
	if string(secret.Value) != "s3cr3t" {
		t.Errorf("secret value = %q, want the stored value verbatim", secret.Value)
	}
	if secret.ContentType != "text/plain" {
		t.Errorf("content type = %q, want text/plain", secret.ContentType)
	}
	if len(secret.Parameters) != 0 {
		t.Errorf("plain session carries %d parameter bytes, want none", len(secret.Parameters))
	}
	if secret.Session != "/org/freedesktop/secrets/session/s1" {
		t.Errorf("secret session = %q, want the passed session", secret.Session)
	}
	if !replace {
		t.Error("replace = false: `vault init` would fail on a duplicate instead of updating")
	}
}

// TestDBusMethodNames pins the service/interface strings the wrappers call:
// a typo'd member name is a runtime D-Bus error no compiler catches.
func TestDBusMethodNames(t *testing.T) {
	if dbusSecretServiceName != "org.freedesktop.secrets" {
		t.Errorf("service name = %q", dbusSecretServiceName)
	}
	if dbusSecretServicePath != "/org/freedesktop/secrets" {
		t.Errorf("service path = %q", dbusSecretServicePath)
	}
	if dbusSecretServiceIface != "org.freedesktop.Secret.Service" {
		t.Errorf("service iface = %q", dbusSecretServiceIface)
	}
	if dbusSecretItemIface != "org.freedesktop.Secret.Item" {
		t.Errorf("item iface = %q", dbusSecretItemIface)
	}
	if dbusCollectionIface != "org.freedesktop.Secret.Collection" {
		t.Errorf("collection iface = %q", dbusCollectionIface)
	}
	if DefaultCollection != "default" {
		t.Errorf("default alias = %q, want default", DefaultCollection)
	}
	if dbusPropertiesIface != "org.freedesktop.DBus.Properties" {
		t.Errorf("properties iface = %q", dbusPropertiesIface)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
