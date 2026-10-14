// Package wasmguest assembles minimal signer guests for tests.
//
// No compiler, no wat2wasm: `go test` is the only tool required. The guest
// exercises all four pulse/env host functions in one signing flow (now_ns,
// header_get, hmac_sha256, log) and appends X-Pulse-Timestamp /
// X-Pulse-Signature to the envelope. Both the in-process interception gate
// (internal/app) and the bare-metal crucible (e2e/) execute this same guest,
// so a signature the unit suite accepts is a signature the binary accepts.
//
// The guest hardcodes the key id "WASM_KEY": the memory map below places it
// at offset 0, and every offset in sign_request is computed from that
// layout. Callers bind the VALUE of WASM_KEY (vault, .env) — never the id.
package wasmguest

// --- minimal wasm assembler (test-only; mirrors internal/wasm/wasmgen_test.go) ---

type wasmAsm struct{ b []byte }

func wuleb(n uint32) []byte {
	var out []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			c |= 0x80
		}
		out = append(out, c)
		if n == 0 {
			return out
		}
	}
}

func wsleb(n int32) []byte {
	var out []byte
	more := true
	for more {
		c := byte(n & 0x7f)
		n >>= 7
		if (n == 0 && c&0x40 == 0) || (n == -1 && c&0x40 != 0) {
			more = false
		} else {
			c |= 0x80
		}
		out = append(out, c)
	}
	return out
}

func (w *wasmAsm) sec(id byte, c []byte) {
	w.b = append(w.b, id)
	w.b = append(w.b, wuleb(uint32(len(c)))...)
	w.b = append(w.b, c...)
}

func wft(params, results []byte) []byte {
	out := []byte{0x60}
	out = append(out, wuleb(uint32(len(params)))...)
	out = append(out, params...)
	out = append(out, wuleb(uint32(len(results)))...)
	out = append(out, results...)
	return out
}

func wvec(chunks ...[]byte) []byte {
	out := wuleb(uint32(len(chunks)))
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func wname(s string) []byte {
	out := wuleb(uint32(len(s)))
	return append(out, s...)
}

// wasm opcodes used by the signing guest.
const (
	wi32Const = 0x41
	wi64Const = 0x42
	wLocalGet = 0x20
	wLocalSet = 0x21
	wLocalTee = 0x22
	wGlobGet  = 0x23
	wGlobSet  = 0x24
	wLoad8U   = 0x2d
	wStore8   = 0x3a
	wBlock    = 0x02
	wLoop     = 0x03
	wEnd      = 0x0b
	wBr       = 0x0c
	wBrIf     = 0x0d
	wReturn   = 0x0f
	wCall     = 0x10
	wSelect   = 0x1b
	wI32Eq    = 0x46
	wI32Ne    = 0x47
	wI32LtS   = 0x48
	wI32LtU   = 0x49
	wI32GeU   = 0x4e
	wI32Add   = 0x6a
	wI32And   = 0x71
	wI32Sub   = 0x6b
	wI32Mul   = 0x6c
	wI32ShrU  = 0x76 // 0x78 is rotr: same stack shape, silently wrong values (see the gate log)
	wI64Sub   = 0x7d
	wI64Mul   = 0x7e
	wI64And   = 0x83
	wI64ShrU  = 0x88 // 0x8a is rotr: harmless here only because every shift is a multiple of 4
	wExtendU  = 0xad
	wWrap     = 0xa7
)

const (
	wi32 = 0x7f
	wi64 = 0x7e
)

func wi32c(b *[]byte, n int32) { *b = append(*b, wi32Const); *b = append(*b, wsleb(n)...) }
func wi64c(b *[]byte, n int64) {
	*b = append(*b, wi64Const)
	v := n
	more := true
	for more {
		c := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && c&0x40 == 0) || (v == -1 && c&0x40 != 0) {
			more = false
		} else {
			c |= 0x80
		}
		*b = append(*b, c)
	}
}
func wlg(b *[]byte, i byte) { *b = append(*b, wLocalGet, i) }
func wls(b *[]byte, i byte) { *b = append(*b, wLocalSet, i) }

// signingGuest assembles the HMAC guest. Static data:
//
//	0: "WASM_KEY" (8)  8: "user-agent" (10)  18: "signed" (6)
//	24: "X-Pulse-Timestamp: " (19)  43: "X-Pulse-Signature: " (19)
//
// sign_request(ptr, len): timestamp via now_ns, hex it, scan the request
// line, MAC ts-hex + LF + request-line under key id "WASM_KEY", require the
// User-Agent header via header_get, log "signed", append both signature
// headers, return the new length. Any host failure returns -1.
func SigningGuest() []byte {
	w := &wasmAsm{b: []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}}
	i32s := []byte{wi32}
	w.sec(1, wvec(
		wft([]byte{wi32, wi32, wi32, wi32, wi32, wi32}, i32s), // 0 hmac
		wft(nil, []byte{wi64}),                                // 1 now_ns
		wft([]byte{wi32, wi32, wi32, wi32}, i32s),             // 2 header_get
		wft([]byte{wi32, wi32}, nil),                          // 3 log
		wft(i32s, i32s),                                       // 4 allocate
		wft([]byte{wi32, wi32}, i32s),                         // 5 sign_request
		wft([]byte{wi32, wi32, wi32}, nil),                    // 6 copy
	))
	imp := func(mod, field string, t uint32) []byte {
		out := wname(mod)
		out = append(out, wname(field)...)
		out = append(out, 0x00)
		return append(out, wuleb(t)...)
	}
	w.sec(2, wvec(
		imp("pulse/env", "hmac_sha256", 0),
		imp("pulse/env", "now_ns", 1),
		imp("pulse/env", "header_get", 2),
		imp("pulse/env", "log", 3),
	))
	w.sec(3, wvec(wuleb(4), wuleb(5), wuleb(6)))
	w.sec(5, []byte{0x01, 0x00, 0x01}) // one memory, min 1 page
	w.sec(6, append(append([]byte{0x01, wi32, 0x01, wi32Const}, wsleb(1024)...), wEnd))
	exp := func(name string, kind byte, idx uint32) []byte {
		out := wname(name)
		out = append(out, kind)
		return append(out, wuleb(idx)...)
	}
	w.sec(7, wvec(
		exp("memory", 0x02, 0),
		exp("allocate", 0x00, 4),
		exp("sign_request", 0x00, 5),
	))

	alloc := []byte{wGlobGet, 0x00, wGlobGet, 0x00, wLocalGet, 0x00, wI32Add, wGlobSet, 0x00, wEnd}

	// copy(dst, src, n): byte loop.
	var copy []byte
	wi32c(&copy, 0)
	wls(&copy, 3)
	copy = append(copy, wBlock, 0x40, wLoop, 0x40)
	wlg(&copy, 3)
	wlg(&copy, 2)
	copy = append(copy, wI32GeU, wBrIf, 0x01)
	wlg(&copy, 0)
	wlg(&copy, 3)
	copy = append(copy, wI32Add)
	wlg(&copy, 1)
	wlg(&copy, 3)
	copy = append(copy, wI32Add, wLoad8U, 0x00, 0x00, wStore8, 0x00, 0x00)
	wlg(&copy, 3)
	wi32c(&copy, 1)
	copy = append(copy, wI32Add)
	wls(&copy, 3)
	copy = append(copy, wBr, 0x00, wEnd, wEnd, wEnd) // loop, block, function body

	// nibChar appends the hex-char sequence for the nibble in local L,
	// leaving [addr c] for store8. Stack in: [addr]. Uses local 12 ($nib).
	nibChar := func(b *[]byte, addrPush func(*[]byte)) {
		addrPush(b)
		wi32c(b, 48)
		wlg(b, 12)
		*b = append(*b, wI32Add)
		wi32c(b, 97)
		wlg(b, 12)
		*b = append(*b, wI32Add)
		wi32c(b, 10)
		*b = append(*b, wI32Sub)
		wlg(b, 12)
		wi32c(b, 10)
		*b = append(*b, wI32LtU, wSelect)
	}

	var sign []byte
	// $ts = now_ns
	sign = append(sign, wCall, 0x01)
	wls(&sign, 2)
	// $tshex = alloc(16)
	wi32c(&sign, 16)
	sign = append(sign, wCall, 0x04)
	wls(&sign, 3)
	// hex loop over 16 nibbles, MSB first
	wi32c(&sign, 0)
	wls(&sign, 4)
	sign = append(sign, wBlock, 0x40, wLoop, 0x40)
	wlg(&sign, 4)
	wi32c(&sign, 16)
	sign = append(sign, wI32Eq, wBrIf, 0x01)
	wlg(&sign, 2) // ts
	wi64c(&sign, 60)
	wlg(&sign, 4)
	sign = append(sign, wExtendU)
	wi64c(&sign, 4)
	sign = append(sign, wI64Mul, wI64Sub, wI64ShrU)
	wi64c(&sign, 15)
	sign = append(sign, wI64And, wWrap)
	wls(&sign, 12)
	nibChar(&sign, func(b *[]byte) { wlg(b, 3); wlg(b, 4); *b = append(*b, wI32Add) })
	sign = append(sign, wStore8, 0x00, 0x00)
	wlg(&sign, 4)
	wi32c(&sign, 1)
	sign = append(sign, wI32Add)
	wls(&sign, 4)
	sign = append(sign, wBr, 0x00, wEnd, wEnd)

	// scan the request line (up to first LF)
	wi32c(&sign, 0)
	wls(&sign, 4)
	sign = append(sign, wBlock, 0x40, wLoop, 0x40)
	wlg(&sign, 4)
	wlg(&sign, 1)
	sign = append(sign, wI32GeU, wBrIf, 0x01)
	wlg(&sign, 0)
	wlg(&sign, 4)
	sign = append(sign, wI32Add, wLoad8U, 0x00, 0x00)
	wi32c(&sign, 10)
	sign = append(sign, wI32Eq, wBrIf, 0x01)
	wlg(&sign, 4)
	wi32c(&sign, 1)
	sign = append(sign, wI32Add)
	wls(&sign, 4)
	sign = append(sign, wBr, 0x00, wEnd, wEnd)
	wlg(&sign, 4)
	wls(&sign, 5) // $linelen

	// $msg = alloc(17 + linelen); msg = tshex + LF + request line
	wlg(&sign, 5)
	wi32c(&sign, 17)
	sign = append(sign, wI32Add, wCall, 0x04)
	wls(&sign, 6)
	wlg(&sign, 6)
	wlg(&sign, 3)
	wi32c(&sign, 16)
	sign = append(sign, wCall, 0x06)
	wlg(&sign, 6)
	wi32c(&sign, 16)
	sign = append(sign, wI32Add)
	wi32c(&sign, 10)
	sign = append(sign, wStore8, 0x00, 0x00)
	wlg(&sign, 6)
	wi32c(&sign, 17)
	sign = append(sign, wI32Add)
	wlg(&sign, 0)
	wlg(&sign, 5)
	sign = append(sign, wCall, 0x06)

	// header_get("user-agent"): fail closed when absent
	wi32c(&sign, 64)
	sign = append(sign, wCall, 0x04)
	wls(&sign, 7)
	wi32c(&sign, 8)
	wi32c(&sign, 10)
	wlg(&sign, 7)
	wi32c(&sign, 64)
	sign = append(sign, wCall, 0x02)
	wls(&sign, 11)
	sign = append(sign, wBlock, 0x40)
	wlg(&sign, 11)
	wi32c(&sign, 0)
	sign = append(sign, wI32LtS, wBrIf, 0x00, wEnd)
	// log("signed")
	wi32c(&sign, 18)
	wi32c(&sign, 6)
	sign = append(sign, wCall, 0x03)

	// hmac("WASM_KEY", msg, 17+linelen) -> $out32; want exactly 32
	wi32c(&sign, 32)
	sign = append(sign, wCall, 0x04)
	wls(&sign, 8)
	wi32c(&sign, 0)
	wi32c(&sign, 8)
	wlg(&sign, 6)
	wlg(&sign, 5)
	wi32c(&sign, 17)
	sign = append(sign, wI32Add)
	wlg(&sign, 8)
	wi32c(&sign, 32)
	sign = append(sign, wCall, 0x00)
	wls(&sign, 11)
	sign = append(sign, wBlock, 0x40)
	wlg(&sign, 11)
	wi32c(&sign, 32)
	sign = append(sign, wI32Ne, wBrIf, 0x00, wEnd)

	// hex-encode 32 bytes -> 64 chars at $hex
	wi32c(&sign, 64)
	sign = append(sign, wCall, 0x04)
	wls(&sign, 9)
	wi32c(&sign, 0)
	wls(&sign, 4)
	sign = append(sign, wBlock, 0x40, wLoop, 0x40)
	wlg(&sign, 4)
	wi32c(&sign, 32)
	sign = append(sign, wI32Eq, wBrIf, 0x01)
	wlg(&sign, 8)
	wlg(&sign, 4)
	sign = append(sign, wI32Add, wLoad8U, 0x00, 0x00)
	wls(&sign, 12) // $nib = byte (reuse)
	// hi char at hex + 2*i (b>>4 is already 0..15, no mask needed)
	wlg(&sign, 12)
	wi32c(&sign, 4)
	sign = append(sign, wI32ShrU)
	wls(&sign, 12)
	nibChar(&sign, func(b *[]byte) {
		wlg(b, 9)
		wlg(b, 4)
		*b = append(*b, wi32Const)
		*b = append(*b, wsleb(2)...)
		*b = append(*b, wI32Mul, wI32Add)
	})
	sign = append(sign, wStore8, 0x00, 0x00)
	// lo char at hex + 2*i + 1
	wlg(&sign, 8)
	wlg(&sign, 4)
	sign = append(sign, wI32Add, wLoad8U, 0x00, 0x00)
	wi32c(&sign, 15)
	sign = append(sign, wI32And)
	wls(&sign, 12)
	nibChar(&sign, func(b *[]byte) {
		wlg(b, 9)
		wlg(b, 4)
		*b = append(*b, wi32Const)
		*b = append(*b, wsleb(2)...)
		*b = append(*b, wI32Mul, wI32Add)
		wi32c(b, 1)
		*b = append(*b, wI32Add)
	})
	sign = append(sign, wStore8, 0x00, 0x00)
	wlg(&sign, 4)
	wi32c(&sign, 1)
	sign = append(sign, wI32Add)
	wls(&sign, 4)
	sign = append(sign, wBr, 0x00, wEnd, wEnd)

	// append at $dst = ptr + len (120 bytes total)
	wlg(&sign, 0)
	wlg(&sign, 1)
	sign = append(sign, wI32Add)
	wls(&sign, 10)
	appendCopy := func(dstOff int32, srcOff int32, n int32) {
		wlg(&sign, 10)
		wi32c(&sign, dstOff)
		sign = append(sign, wI32Add)
		wi32c(&sign, srcOff)
		wi32c(&sign, n)
		sign = append(sign, wCall, 0x06)
	}
	appendCopy(0, 24, 19) // "X-Pulse-Timestamp: "
	// tshex lives in guest memory, not statics: copy it by address.
	wlg(&sign, 10)
	wi32c(&sign, 19)
	sign = append(sign, wI32Add)
	wlg(&sign, 3)
	wi32c(&sign, 16)
	sign = append(sign, wCall, 0x06)
	// LF after timestamp
	wlg(&sign, 10)
	wi32c(&sign, 35)
	sign = append(sign, wI32Add)
	wi32c(&sign, 10)
	sign = append(sign, wStore8, 0x00, 0x00)
	// "X-Pulse-Signature: " + hex + LF
	wlg(&sign, 10)
	wi32c(&sign, 36)
	sign = append(sign, wI32Add)
	wi32c(&sign, 43)
	wi32c(&sign, 19)
	sign = append(sign, wCall, 0x06)
	wlg(&sign, 10)
	wi32c(&sign, 55)
	sign = append(sign, wI32Add)
	wlg(&sign, 9)
	wi32c(&sign, 64)
	sign = append(sign, wCall, 0x06)
	wlg(&sign, 10)
	wi32c(&sign, 119)
	sign = append(sign, wI32Add)
	wi32c(&sign, 10)
	sign = append(sign, wStore8, 0x00, 0x00)
	// return len + 120
	wlg(&sign, 1)
	wi32c(&sign, 120)
	sign = append(sign, wI32Add, wEnd)

	code := func(body []byte, groups ...[2]byte) []byte {
		inner := wuleb(uint32(len(groups)))
		for _, g := range groups {
			inner = append(inner, g[0], g[1])
		}
		inner = append(inner, body...)
		out := wuleb(uint32(len(inner)))
		return append(out, inner...)
	}
	w.sec(10, wvec(
		code(alloc),
		code(sign, [2]byte{0x01, 0x7e}, [2]byte{0x0a, 0x7f}),
		code(copy, [2]byte{0x01, 0x7f}),
	))
	statics := []byte("WASM_KEY" + "user-agent" + "signed" + "X-Pulse-Timestamp: " + "X-Pulse-Signature: ")
	if len(statics) != 62 {
		panic("static layout drift")
	}
	seg := append([]byte{0x00, wi32Const}, wsleb(0)...)
	seg = append(seg, wEnd)
	seg = append(seg, wuleb(uint32(len(statics)))...)
	seg = append(seg, statics...)
	w.sec(11, wvec(seg))
	return w.b
}
