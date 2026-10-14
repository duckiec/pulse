package wasm

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

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

// Host function names under HostModuleName. Together with HostFuncHMACSHA256
// they are the CLOSED import whitelist (ROADMAP.md §3.4.3): a guest declaring
// any other import fails to instantiate with ErrIsolationViolation.
const (
	// HostFuncNowNS is now_ns() -> i64: wall-clock nanoseconds for
	// timestamped auth headers.
	HostFuncNowNS = "now_ns"
	// HostFuncHeaderGet is header_get(name_ptr, name_len, out_ptr, out_cap)
	// -> i32: read-only access to the pre-flight request headers attached to
	// the call context via WithRequest. Returns bytes written, -1 on any
	// failure (bad range, no request attached, unknown or sensitive header).
	HostFuncHeaderGet = "header_get"
	// HostFuncLog is log(ptr, len): a bounded diagnostic sink. It writes to
	// logSink (default os.Stderr), NEVER to os.Stdout: in MCP mode stdout is
	// the JSON-RPC channel and a stray byte corrupts the stream (AGENTS.md
	// §3.3). Over-long messages are truncated, out-of-bounds ones dropped.
	HostFuncLog = "log"
)

// maxLogBytes bounds one guest log message. A diagnostic sink without a bound
// is a memory pipe a malicious guest can hold open.
const maxLogBytes = 4096

// requestKey carries the pre-flight *http.Request for header_get. It is
// unexported so only WithRequest can set it; a guest can never forge it.
type requestKey struct{}

// WithRequest attaches the pre-flight request to ctx so the header_get host
// function can serve it. The host never mutates it: the value is read-only
// access, and the signed headers travel back through the memory ABI.
func WithRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, requestKey{}, r)
}

// requestFromContext returns the attached request, or nil when the caller did
// not attach one. A nil here fails the header_get call closed (-1), never
// with a Header map the guest should not see.
func requestFromContext(ctx context.Context) *http.Request {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(requestKey{}).(*http.Request)
	return r
}

// redactedHeaders are header names whose VALUES a guest must never see.
// header_get answers them with "[REDACTED]": the guest learns the header
// exists (so a signature can attest to its presence) without learning the
// secret it carries. The set mirrors the engine's sensitiveHeaders; it is
// duplicated rather than imported because sibling imports are forbidden
// (AGENTS.md §3.1) and this list is a security boundary that must read
// explicitly at the enforcement site.
var redactedHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
}

// redactedValue is what a sensitive header reads as through header_get.
const redactedValue = "[REDACTED]"

// logSink is where the log host function writes. It is a variable (not a
// const, not os.Stdout) so tests can capture it — and so the invariant reads
// as code: nothing in this file names os.Stdout, ever.
var logSink interface {
	Write([]byte) (int, error)
} = os.Stderr

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

// newNowNSFunc returns the Go side of the now_ns ABI: () -> i64.
//
// Wall-clock nanoseconds. The sandbox denies the guest a clock by never
// offering WASI or syscalls; this single i64 is the ONLY time a signing script
// can observe, and it is what makes an HMAC timestamped rather than replayable.
func newNowNSFunc() api.GoModuleFunc {
	return api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		stack[0] = uint64(time.Now().UnixNano())
	})
}

// newHeaderGetFunc returns the Go side of the header_get ABI:
// header_get(name_ptr, name_len, out_ptr, out_cap) -> i32.
//
// It returns the bytes written, or -1 on ANY failure: a range outside linear
// memory, no request attached to the context, or an unknown header. Sensitive
// headers (Authorization, Cookie, ...) read as "[REDACTED]" — presence
// without content, so a signature can attest to a header the guest may not
// exfiltrate. The request itself is never mutated and never handed over.
func newHeaderGetFunc() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		mem := mod.Memory()
		if mem == nil {
			stack[0] = failCode()
			return
		}
		namePtr := uint32(stack[0])
		nameLen := uint32(stack[1])
		outPtr := uint32(stack[2])
		outCap := uint32(stack[3])

		if !inBounds(mem, namePtr, nameLen) || !inBounds(mem, outPtr, outCap) {
			stack[0] = failCode()
			return
		}
		nameView, ok := mem.Read(namePtr, nameLen)
		if !ok {
			stack[0] = failCode()
			return
		}
		req := requestFromContext(ctx)
		if req == nil {
			stack[0] = failCode()
			return
		}
		name := string(append([]byte(nil), nameView...))
		val := req.Header.Get(name)
		if val == "" {
			// Unknown header, or present-but-empty: indistinguishable by
			// design, so a guest cannot probe for header names by length.
			stack[0] = failCode()
			return
		}
		if redactedHeaders[strings.ToLower(name)] {
			val = redactedValue
		}
		if uint32(len(val)) > outCap {
			stack[0] = failCode()
			return
		}
		if !mem.Write(outPtr, []byte(val)) {
			stack[0] = failCode()
			return
		}
		stack[0] = uint64(len(val))
	})
}

// newLogFunc returns the Go side of the log ABI: log(ptr, len).
//
// Bounded (truncated at maxLogBytes) and stderr-only by construction: the
// sink is logSink, which defaults to os.Stderr and is never os.Stdout. An
// out-of-bounds range is dropped silently — log returns nothing, so there is
// no channel to report on, and a diagnostic that could crash the host would
// be worse than a diagnostic lost.
func newLogFunc() api.GoModuleFunc {
	return api.GoModuleFunc(func(_ context.Context, mod api.Module, stack []uint64) {
		mem := mod.Memory()
		if mem == nil {
			return
		}
		ptr := uint32(stack[0])
		n := uint32(stack[1])
		if !inBounds(mem, ptr, n) {
			return
		}
		view, ok := mem.Read(ptr, n)
		if !ok {
			return
		}
		msg := append([]byte(nil), view...)
		if len(msg) > maxLogBytes {
			msg = msg[:maxLogBytes]
		}
		_, _ = fmt.Fprintf(logSink, "pulse: wasm: %s\n", msg)
	})
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
