package wasm

// Gates for the request-hook ABI (`handle_request`) and the isolation boundary
// a hook shares with the signer.
//
// The hook guest is hand-assembled for the same reason every other guest here
// is: no compiler or wat2wasm dependency, so `go test` remains the only tool
// required. It overwrites the payload with a fixed JSON document, which is
// enough to prove the host writes the request in, takes the guest's bytes out,
// and enforces the same length contract as signing.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fixedHook builds a guest exporting memory/allocate/handle_request.
//
//	(module
//	  (memory (export "memory") 1)
//	  (global $bump (mut i32) (i32.const 1024))
//	  (func (export "allocate") (param $size i32) (result i32) ...)
//	  (func (export "handle_request") (param $ptr i32) (param $size i32) (result i32)
//	    ;; memory[$ptr + i] = payload[i] for every byte, unrolled
//	    i32.const len(payload)))
//
// It exports NO sign_request, which also proves the sandbox no longer demands
// both ABIs from every guest.
func fixedHook(payload []byte) []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),
	)
	w.funcs(0, 1)
	w.memory(4)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABIHandleRequest, 0x00, 1),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	hook := make([]byte, 0, len(payload)*10+8)
	for i, b := range payload {
		hook = append(hook, opLocalGet, 0x00) // ptr
		hook = append(hook, opI32Const)
		hook = append(hook, sleb(int32(i))...)
		hook = append(hook, opI32Add)
		hook = append(hook, opI32Const)
		hook = append(hook, sleb(int32(b))...)
		hook = append(hook, opI32Store8, 0x00, 0x00)
	}
	hook = append(hook, opI32Const)
	hook = append(hook, sleb(int32(len(payload)))...)
	hook = append(hook, opEnd)
	w.code(alloc, hook)
	return w.bytes()
}

// TestHookRoundTripIsTheHostContract drives one hook call end to end: the host
// reserves memory, writes the JSON request, calls handle_request with the
// PAYLOAD length, and reads back exactly the document the guest reported.
func TestHookRoundTripIsTheHostContract(t *testing.T) {
	out := []byte(`{"method":"POST","url":"https://api.example.com/v1","headers":[["Authorization","Bearer signed"]],"body":""}`)
	m := newModule(t, Config{Wasm: fixedHook(out)})
	if m.CanSign() {
		t.Fatal("a hook-only guest reports CanSign")
	}
	if !m.CanHook() {
		t.Fatal("a hook guest reports CanHook=false")
	}

	got, err := m.HandleRequest(context.Background(), []byte(`{"method":"GET","url":"https://api.example.com/v1","headers":[]}`))
	if err != nil {
		t.Fatalf("HandleRequest: %v", err)
	}
	if string(got) != string(out) {
		t.Fatalf("hook output = %q, want %q", got, out)
	}
	// The same module must still refuse the signing ABI it did not export, so a
	// caller that asks for the wrong one gets direction rather than a trace.
	if _, err := m.Sign(context.Background(), []byte("GET /x")); !errors.Is(err, ErrABIViolation) {
		t.Fatalf("Sign on a hook-only guest = %v, want ErrABIViolation", err)
	}
}

// envGuest asks for the HOST ENVIRONMENT: env.getenv(name,len,cap) -> i32.
// It is the import a script would use to read PULSE_VAULT_FILE, and reading
// /etc/passwd would use the env.read_file case beside it. Neither module exists
// in the sandbox, so both requests die at instantiation.
func envGuest() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32, typeI32, typeI32}, []byte{typeI32}), // getenv
		funcType([]byte{typeI32}, []byte{typeI32}),                   // allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),          // handle_request
	)
	w.importSection(importEntry("env", "getenv", 0))
	w.funcs(1, 2)
	w.memory(4)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 1),
		export(ABIHandleRequest, 0x00, 2),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	hook := []byte{opI32Const, 0x00, opEnd}
	w.code(alloc, hook)
	return w.bytes()
}

// TestHookIsolationIsStructural is the capability boundary the feature exists
// for: a hook is untrusted user code, and filesystem, environment and WASI
// imports are refused because no host module is ever registered. Pulse's own
// vault pointer is an ordinary environment variable — there is no environment
// to read, so naming PULSE_VAULT_FILE (or /etc/passwd) buys a guest nothing.
func TestHookIsolationIsStructural(t *testing.T) {
	cases := []struct {
		name string
		wasm []byte
	}{
		{"an env.read_file import (/etc/passwd)", importingSigner()},
		{"an env.getenv import (PULSE_VAULT_FILE)", envGuest()},
		{"a WASI fd_write import", wasiGuest()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := New(context.Background(), Config{Wasm: c.wasm})
			if err == nil {
				_ = m.Close(context.Background())
				t.Fatalf("a guest declaring %s instantiated", c.name)
			}
			if !errors.Is(err, ErrIsolationViolation) {
				t.Fatalf("error = %v (%T), want ErrIsolationViolation", err, err)
			}
			if strings.Contains(strings.ToLower(err.Error()), "pulse_vault_file") {
				t.Fatalf("the refusal echoed an environment name: %v", err)
			}
		})
	}
}

// TestHookOutputLengthIsChecked proves the hook path shares the signer's memory
// bound rather than trusting the guest: a module claiming more output than the
// reservation is refused without reading a byte.
func TestHookOutputLengthIsChecked(t *testing.T) {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),
	)
	w.funcs(0, 1)
	w.memory(4)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABIHandleRequest, 0x00, 1),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	hook := []byte{opI32Const}
	hook = append(hook, sleb(1<<20)...) // far beyond payload+headroom
	hook = append(hook, opEnd)
	w.code(alloc, hook)

	m := newModule(t, Config{Wasm: w.bytes()})
	if _, err := m.HandleRequest(context.Background(), []byte(`{"method":"GET"}`)); !errors.Is(err, ErrABIViolation) {
		t.Fatalf("lying hook = %v, want ErrABIViolation", err)
	}
}

// TestAGuestMustImplementAtLeastOneABI closes the laxity introduced by making
// each ABI optional: a module with neither is still refused at load.
func TestAGuestMustImplementAtLeastOneABI(t *testing.T) {
	w := newWasm()
	w.types(funcType([]byte{typeI32}, []byte{typeI32}))
	w.funcs(0)
	w.memory(4)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
	)
	w.code([]byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	})
	m, err := New(context.Background(), Config{Wasm: w.bytes()})
	if err == nil {
		_ = m.Close(context.Background())
		t.Fatal("a guest exporting neither sign_request nor handle_request was accepted")
	}
	if !strings.Contains(err.Error(), ABIHandleRequest) {
		t.Fatalf("error = %v, want it to name both ABIs", err)
	}
}
