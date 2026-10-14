package wasm

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
)

// stubResolver is the in-test vault.
type stubResolver struct{ secret string }

func (s stubResolver) Resolve(key string) (string, error) {
	if key == "" {
		return "", context.Canceled
	}
	return s.secret, nil
}

// hmacGuest builds a guest that imports the zero-trust host function and
// invokes it with three fixed buffers. The guest itself never sees the key.
func hmacGuest() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32, typeI32, typeI32, typeI32, typeI32, typeI32}, []byte{typeI32}), // host hmac
		funcType([]byte{typeI32}, []byte{typeI32}),          // allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}), // sign_request
		funcType(nil, []byte{typeI32}),                      // run
	)
	w.importSection(importEntry(HostModuleName, HostFuncHMACSHA256, 0))
	w.funcs(1, 2, 3)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 1),
		export(ABISignRequest, 0x00, 2),
		export("run", 0x00, 3),
	)

	alloc := []byte{opGlobalGet, 0, opGlobalGet, 0, opLocalGet, 0, opI32Add, opGlobalSet, 0, opEnd}
	sign := []byte{opI32Const, 0, opEnd}

	// run(): push the six i32 arguments, call the import, return its result.
	body := []byte{
		opI32Const, 0,   // key_ptr
		opI32Const, 7,   // key_len (key-id is "key-id\0"? no, "key-id" = 6? adjust below)
	}
	// We instead write the key id bytes and pass its true length from Go via
	// constants fixed here: "key-1" is 5 bytes. Rebuild constants correctly.
	body = []byte{}
	body = append(body, opI32Const)
	body = append(body, sleb(0)...)
	body = append(body, opI32Const)
	body = append(body, sleb(5)...)
	body = append(body, opI32Const)
	body = append(body, sleb(64)...)
	body = append(body, opI32Const)
	body = append(body, sleb(5)...)
	body = append(body, opI32Const)
	body = append(body, sleb(128)...)
	body = append(body, opI32Const)
	body = append(body, sleb(64)...)
	body = append(body, opCall, 0)
	body = append(body, opEnd)

	w.code(alloc, sign, body)
	return w.bytes()
}

func TestZeroTrustHostGate(t *testing.T) {
	const secret = "topsecret-vault-value"
	const keyID = "key-1"

	m := newModule(t, Config{
		Wasm:            hmacGuest(),
		Resolver:        stubResolver{secret: secret},
		Name:            "host-hmac-guest",
	})

	mem := m.Memory()
	// Lay out the three buffers the guest will reference.
	if !mem.Write(0, []byte(keyID)) {
		t.Fatal("write key id")
	}
	for i := len(keyID); i < 64; i++ {
		mem.Write(uint32(i), []byte{0}) // ensure zero padding
	}
	if !mem.Write(64, []byte("hello")) {
		t.Fatal("write payload")
	}

	fn := m.mod.ExportedFunction("run")
	res, err := fn.Call(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res) != 1 || int32(res[0]) != 32 {
		t.Fatalf("host returned %v, want 32", res)
	}

	got := make([]byte, 32)
	view, ok := mem.Read(128, 32)
	if !ok {
		t.Fatal("read signature")
	}
	copy(got, view)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("hello"))
	want := mac.Sum(nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("signature mismatch:\n got %x\nwant %x", got, want)
	}
	t.Logf("HMAC-SHA256 computed by host: %x", got)

	// Zero-trust proof: the secret must not appear anywhere in guest memory.
	full := make([]byte, mem.Size())
	v, ok := mem.Read(0, uint32(len(full)))
	if !ok {
		t.Fatal("read guest memory")
	}
	copy(full, v)
	if bytes.Contains(full, []byte(secret)) {
		t.Fatalf("ZERO-TRUST VIOLATION: raw secret %q found in guest memory", secret)
	}
	t.Log("zero-trust: raw secret absent from guest memory")

	// Also prove the key id (handle) IS allowed to appear, so the test is
	// actually exercising the path rather than an empty memory.
	if !bytes.Contains(full, []byte(keyID)) {
		t.Fatal("key id handle missing from guest memory; test setup is wrong")
	}
}

// TestHostFunctionIsRegistered proves a guest importing the host module
// instantiates successfully when a resolver is supplied, and is refused
// without one.
func TestHostFunctionIsRegistered(t *testing.T) {
	m := newModule(t, Config{Wasm: hmacGuest(), Resolver: stubResolver{secret: "x"}})
	if m == nil {
		t.Fatal("module with resolver did not build")
	}
	// Without a resolver, a guest declaring the import must fail to
	// instantiate, preserving the default-deny property.
	if _, err := New(context.Background(), Config{Wasm: hmacGuest()}); err == nil {
		t.Fatal("a guest importing host functions instantiated without a resolver")
	}
}
