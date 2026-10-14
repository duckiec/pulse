package wasm

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"

	"github.com/tetratelabs/wazero/api"
)

// HostModuleName is the wazero module name under which the zero-trust host
// functions are registered. A guest that wants HMAC must import from it.
const HostModuleName = "pulse/env"

// HostFuncHMACSHA256 is the single host function §3.4.3 requires: the guest
// supplies a key id and a payload (both as in-guest memory ranges) plus an
// output range, and the host writes the HMAC-SHA256 back. The key material
// never crosses into guest addressable memory in either direction.
const HostFuncHMACSHA256 = "hmac_sha256"

// SecretResolver is the contract the host function depends on. It is the same
// shape as secrets.SecretResolver; keeping it local stops this package from
// importing a sibling and lets a test substitute a stub.
type SecretResolver interface {
	Resolve(key string) (string, error)
}

// newHMACHostFunc returns the Go side of the HMAC-by-key-id ABI.
//
// ABI:
//
//	hmac_sha256(key_id_ptr, key_id_len,
//	            msg_ptr, msg_len,
//	            out_ptr, out_cap) -> i32
//
// Returns the number of bytes written to out_ptr (32 on success), -1 on any
// failure (unresolved key, short output buffer, bad range). The host resolves
// the key by reading the key id bytes out of guest memory, computes the MAC
// in Go, and writes only the 32-byte digest into guest memory. The secret
// itself is never written anywhere the guest can address.
func newHMACHostFunc(resolver SecretResolver) api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		mem := mod.Memory()
		if mem == nil {
			stack[0] = failCode()
			return
		}

		keyPtr := uint32(stack[0])
		keyLen := uint32(stack[1])
		msgPtr := uint32(stack[2])
		msgLen := uint32(stack[3])
		outPtr := uint32(stack[4])
		outCap := uint32(stack[5])

		// Bounds checks before any read: a bogus pointer must not even reach
		// the slice view.
		if !inBounds(mem, keyPtr, keyLen) || !inBounds(mem, msgPtr, msgLen) ||
			!inBounds(mem, outPtr, outCap) {
			stack[0] = failCode()
			return
		}

		keyView, ok := mem.Read(keyPtr, keyLen)
		if !ok {
			stack[0] = failCode()
			return
		}
		keyID := string(append([]byte(nil), keyView...))

		msgView, ok := mem.Read(msgPtr, msgLen)
		if !ok {
			stack[0] = failCode()
			return
		}
		msg := append([]byte(nil), msgView...)

		secret, err := resolver.Resolve(keyID)
		if err != nil {
			stack[0] = failCode()
			return
		}

		// The host holds the secret only long enough to key the MAC. It is
		// never copied into guest memory, never returned, never logged.
		h := hmac.New(sha256.New, []byte(secret))
		_, _ = h.Write(msg)
		sig := h.Sum(nil)

		// The output buffer is guest-owned; the host may only write within it.
		if uint32(len(sig)) > outCap {
			stack[0] = failCode()
			return
		}
		if !mem.Write(outPtr, sig) {
			stack[0] = failCode()
			return
		}
		stack[0] = uint64(len(sig))
	}) // close GoModuleFunc and newHMACHostFunc
}

// inBounds reports whether [ptr, ptr+size) stays inside linear memory. The
// subtraction form guards against uint32 wrap, matching internal/wasm's own
// checkRange discipline.
func inBounds(mem api.Memory, ptr, size uint32) bool {
	avail := mem.Size()
	if ptr > avail {
		return false
	}
	if size > avail-ptr {
		return false
	}
	return true
}

func failCode() uint64 { return 0xFFFFFFFF } // -1 as an i32
