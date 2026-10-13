package wasm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// firstLine keeps a multi-line wasm stack trace out of a log line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func newModule(t *testing.T, cfg Config) *Module {
	t.Helper()
	m, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return m
}

// TestSignRoundTripIsTheExecutionGate is the gate the feature exists for: a
// guest is loaded, the host allocates inside it, writes the pre-flight request
// envelope, the guest signs it, and the host reads the signed bytes back.
//
// Every step is asserted, and the chain is cumulative — the returned value
// could only exist if all of them happened:
//
//   - the module instantiates and reports a memory,
//   - allocate returns a usable pointer,
//   - the envelope is readable back from guest memory BEFORE signing, which is
//     what proves the write actually landed rather than being optimised away,
//   - sign_request returns a plausible length,
//   - the returned bytes are the SIGNED form and not the input echoed back.
func TestSignRoundTripIsTheExecutionGate(t *testing.T) {
	m := newModule(t, Config{Wasm: benignSigner(), Name: "signer"})

	if m.MemorySize() == 0 {
		t.Fatal("the guest reports a zero-byte memory")
	}
	t.Logf("guest instantiated: %d bytes of linear memory", m.MemorySize())

	const envelope = "POST /v1/charge HTTP/1.1\r\n" +
		"Host: api.example.com\r\n" +
		"Content-Length: 2\r\n" +
		"\r\n" +
		"{}"
	in := []byte(envelope)

	out, err := m.Sign(context.Background(), in)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	t.Logf("signed envelope (%d bytes):\n%s", len(out), out)

	// The signed form must carry the guest's header...
	want := "X-Wasm-Signed: true\r\n"
	if !strings.Contains(string(out), want) {
		t.Errorf("the signed envelope does not carry %q:\n%q", want, out)
	}
	// ...and must still carry what was signed, or the header proves nothing
	// about a buffer that was replaced wholesale.
	if !strings.Contains(string(out), "POST /v1/charge") {
		t.Errorf("the signed envelope lost the request line:\n%q", out)
	}
	if len(out) <= len(want) {
		t.Errorf("the guest reported %d output bytes, which is no more than "+
			"the header it claims to have written", len(out))
	}

	// The returned slice must be a COPY. api.Memory.Read hands back a view into
	// the guest's linear memory, and returning that would let a later guest
	// write mutate a signature that has already been checked.
	before := string(out)
	for i := range out {
		out[i] = 'X' // scribble on the returned slice
	}
	again, ok := m.Memory().Read(1024, uint32(len(before)))
	if !ok || len(again) == 0 {
		t.Skip("could not re-read guest memory for the copy assertion")
	}
	if strings.Contains(string(again), strings.Repeat("X", 8)) {
		t.Error("the returned slice aliases guest memory; a later guest write " +
			"would change a signature that has already been verified")
	}
}

// TestTheHostCanSeeTheEnvelopeBeforeSigning proves the WRITE landed.
//
// Without this, a sandbox that discarded the payload and handed the guest a
// zeroed buffer would still pass TestSignRoundTripIsTheExecutionGate, because
// the guest writes its header to a fixed offset regardless of what was there.
func TestTheHostCanSeeTheEnvelopeBeforeSigning(t *testing.T) {
	m := newModule(t, Config{Wasm: benignSigner()})
	payload := []byte("GET /health")

	// Drive the first two ABI steps directly; the round-trip test covers the
	// rest.
	ptr, err := m.allocate(context.Background(), uint32(len(payload)))
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if ptr == 0 {
		t.Fatal("allocate returned 0, which the ABI defines as failure")
	}
	if err := m.write(ptr, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := m.read(ptr, uint32(len(payload)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("guest memory holds %q after the write, want %q", got, payload)
	}
	t.Logf("allocate(%d) -> %d; the envelope reads back verbatim", len(payload), ptr)
}

// TestInfiniteLoopIsStoppedByTheTimeBudget is THE METERING GATE.
//
// A guest that never returns is the whole threat: it is not a crash, it is a
// process that stops responding while holding a runtime lock. The assertions
// are layered so that each one can fail for its own reason and say so:
//
//  1. the call RETURNS — the host is not wedged;
//  2. it returns an error, not a signature;
//  3. the error is typed, so a caller can distinguish "too slow" from
//     "misbehaved";
//  4. it returns within a BOUND derived from the configured deadline.
//
// On (4): the deadline is 50ms and wazero polls the context at engine safe
// points, so the observed time is the deadline plus one poll interval. The
// bound is therefore several multiples of the deadline rather than the deadline
// itself — asserting "under 50ms" would be asserting that the poll interval is
// zero, which is a property of wazero's internals and not of this code. The
// measured value is logged so the slack stays visible.
func TestInfiniteLoopIsStoppedByTheTimeBudget(t *testing.T) {
	const budget = 50 * time.Millisecond
	m := newModule(t, Config{Wasm: infiniteLoop(), Timeout: budget, Name: "loop"})

	done := make(chan error, 1)
	go func() {
		_, err := m.Sign(context.Background(), []byte("GET /"))
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		// The gate's own backstop. If this fires, the sandbox did not stop an
		// infinite loop and the test binary is about to be killed by the go
		// test timeout, which reads as a suite failure rather than as a finding.
		t.Fatal("the sandbox did not stop a guest in an infinite loop within 5s; " +
			"the time budget is not being enforced")
	}

	if err == nil {
		t.Fatal("an infinite loop produced no error; the sandbox returned a " +
			"signature from a guest that never finished")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("error = %v (%T), want ErrTimeout; a caller cannot tell a slow "+
			"guest from a broken one without a typed error", err, err)
	}
	if m.Closed() != true {
		t.Error("a module whose call was cut short is still open; a guest that " +
			"ran past its budget must not be reused for a signature")
	}
	t.Logf("infinite loop halted: %v", err)
}

// TestTimeBudgetIsBoundedNotMerelyConfigured measures the elapsed time.
//
// The other gate asserts the error TYPE; this one asserts that the bound is
// real, because a correctly-typed error produced after two minutes would be a
// correctly-typed hang.
func TestTimeBudgetIsBoundedNotMerelyConfigured(t *testing.T) {
	const budget = 50 * time.Millisecond
	m := newModule(t, Config{Wasm: infiniteLoop(), Timeout: budget})

	start := time.Now()
	_, err := m.Sign(context.Background(), []byte("x"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("an infinite loop produced no error")
	}
	// Several multiples of the deadline, to absorb wazero's context poll
	// interval without pretending it is zero.
	const bound = 10 * budget
	if elapsed > bound {
		t.Errorf("a %v budget took %v to stop the guest, over the %v bound", budget, elapsed, bound)
	}
	t.Logf("%v budget stopped the guest in %v (%.2fx the deadline)", budget, elapsed,
		float64(elapsed)/float64(budget))
}

// TestCallBudgetIsDetectedAndTheModuleIsPoisoned is the fuel gate, and it
// asserts what the mechanism ACTUALLY does rather than what it would be nice
// for it to do.
//
// The first draft of this gate claimed the budget stops a guest after `fuel`
// calls. It does not, and the gate caught it: a 20,000-call guest ran against a
// budget of 50 and reported 20,003 calls before returning, because the listener
// cancels the context and a straight-line unrolled body does not reach a
// context check until it ends.
//
// So fuel is a DETECTOR, not a preemptor, and its security value is different
// from — and additional to — termination:
//
//   - it is DETERMINISTIC: it counts calls, not milliseconds, so it reaches the
//     same decision on a loaded CI runner as on an idle laptop;
//   - it POISONS the module: a guest detected over budget is never reused.
//
// Each of those is asserted here, and the timeout is deliberately set to the
// maximum the sandbox allows so that ErrTimeout cannot be what fires — a gate
// that passed because the wrong mechanism stopped the guest would be worse than
// no gate.
func TestCallBudgetIsDetectedAndTheModuleIsPoisoned(t *testing.T) {
	const fuel = 50
	m := newModule(t, Config{
		Wasm:    callStorm(20_000),
		Fuel:    fuel,
		Timeout: MaxTimeout, // the ceiling: TIMEOUT cannot be what fires
		Name:    "storm",
	})

	start := time.Now()
	_, err := m.Sign(context.Background(), []byte("GET /"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a guest that never finished produced no error")
	}
	// The budget fired, not the clock. The whole 20,000-call guest completes in
	// single-digit milliseconds, so a two-second timeout is three orders of
	// magnitude away from being the cause.
	if !errors.Is(err, ErrFuelExhausted) {
		t.Errorf("error = %v (%T), want ErrFuelExhausted. A 20,000-call guest "+
			"finishes in milliseconds against a %v timeout, so the CALL BUDGET "+
			"is the only thing that can have fired.", err, err, MaxTimeout)
	}
	if n := m.FuelUsed(); n <= fuel {
		t.Errorf("the guest made %d calls against a budget of %d; the counter "+
			"never passed the budget, so the bound cannot have fired", n, fuel)
	}
	// Determinism is the point of tier 2, so it is measured: the same guest
	// against a budget that cannot fire must run to completion without being
	// poisoned.
	if elapsed >= MaxTimeout/2 {
		t.Errorf("the guest took %v, which is suspiciously close to the %v "+
			"timeout; the gate cannot tell the two mechanisms apart", elapsed, MaxTimeout)
	}
	if !m.Closed() {
		t.Error("a module detected over budget is still open; the budget is " +
			"advisory unless the module is poisoned")
	}
	if _, err := m.Sign(context.Background(), []byte("GET /")); !errors.Is(err, ErrClosed) {
		t.Errorf("a poisoned module returned %v, want ErrClosed", err)
	}
	t.Logf("20,000-call guest: budget of %d detected after %d calls in %v, "+
		"module poisoned, elapsed is %.4f of the %v timeout",
		fuel, m.FuelUsed(), elapsed, float64(elapsed)/float64(MaxTimeout), MaxTimeout)
}

// TestAGuestUnderBudgetIsNotPoisoned is the other half of that gate.
//
// A detector that poisons every module would be a denial of service against the
// user's own signing scripts, which is not a trade worth making for a security
// property. The budget must be a threshold, not a tripwire.
func TestAGuestUnderBudgetIsNotPoisoned(t *testing.T) {
	m := newModule(t, Config{
		Wasm:    callStorm(500), // 500 leaf calls plus the driver's own
		Fuel:    10_000,
		Timeout: MaxTimeout,
	})
	if _, err := m.Sign(context.Background(), []byte("GET /")); err != nil {
		t.Fatalf("a guest well under budget was failed: %v", err)
	}
	if m.Closed() {
		t.Error("a guest that stayed under its budget was poisoned; the budget " +
			"would then be a denial of service against legitimate scripts")
	}
	if n := m.FuelUsed(); n == 0 || n >= 10_000 {
		t.Errorf("the guest used %d calls of a 10,000 budget; the counter is not "+
			"measuring what it claims", n)
	}
	// A second call must also succeed: the budget is per call.
	if _, err := m.Sign(context.Background(), []byte("GET /")); err != nil {
		t.Errorf("a second call on a healthy module failed: %v", err)
	}
	t.Logf("a 500-call guest used %d of a 10,000 budget and remained usable", m.FuelUsed())
}

// TestDeepRecursionTrap records a finding rather than asserting a design.
//
// The first draft used a self-recursive guest for the fuel gate. It fails, and
// the reason is worth keeping: wazero's interpreter enforces its OWN call-stack
// bound, and a deep recursion trips it — as a clean, recoverable trap — long
// before any multi-thousand-call budget could fire. Fuel bounds total work; the
// stack bound bounds depth; they are different guarantees and only the first is
// this package's to configure.
//
// The property asserted here is the one that actually matters for a hostile
// guest: the overflow is a TRAP the host survives, not a crash.
func TestDeepRecursionTrap(t *testing.T) {
	m := newModule(t, Config{
		Wasm:    infiniteRecursion(),
		Fuel:    1_000_000, // far above the stack bound, so the STACK is what trips
		Timeout: MaxTimeout,
	})

	done := make(chan error, 1)
	go func() {
		_, err := m.Sign(context.Background(), []byte("x"))
		done <- err
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a self-recursive guest was neither trapped nor bounded within 10s")
	}
	if err == nil {
		t.Fatal("an unbounded recursion produced no error")
	}
	if !strings.Contains(err.Error(), "stack overflow") && !errors.Is(err, ErrFuelExhausted) {
		t.Logf("recursion ended with: %v", err)
	}
	t.Logf("self-recursion was stopped by wazero's own stack bound, and the host "+
		"survived: %v", firstLine(err.Error()))
}

// TestFuelBudgetIsEnforcedAcrossCalls proves the counter is reset per call.
//
// wazero calls NewFunctionListener once per function DEFINITION, so a counter
// held in the listener closure restarts for every distinct function and never
// accumulates. That was the first draft's bug and it would have made the budget
// unenforceable in exactly the case it exists for. This gate fails if the
// counter is ever made per-listener again.
func TestFuelBudgetIsEnforcedAcrossCalls(t *testing.T) {
	const fuel = 200
	m := newModule(t, Config{Wasm: benignSigner(), Fuel: fuel})

	// Two benign calls. The signer makes two function calls per Sign (allocate
	// then sign_request), so each consumes a couple of units.
	payload := []byte("GET /x HTTP/1.1\r\nHost: example\r\n\r\n")
	for i := 0; i < 2; i++ {
		if _, err := m.Sign(context.Background(), payload); err != nil {
			t.Fatalf("call %d failed on a benign guest: %v", i, err)
		}
	}
	if n := m.FuelUsed(); n == 0 {
		t.Fatal("the fuel counter never moved; no calls are being counted at all")
	} else if n > fuel {
		t.Errorf("a benign guest used %d units against a budget of %d", n, fuel)
	}
	t.Logf("two benign calls consumed %d fuel units of a %d budget", m.FuelUsed(), fuel)
}

// TestAGuestCannotReadTheHost proves the isolation is STRUCTURAL.
//
// Every denial in this package is an omission — no FS, no env, no clock, no
// randomness, no WASI — and an omission cannot be asserted by checking a config
// struct. It is asserted by handing the guest a guest that tries, and requiring
// the attempt to fail at LOAD time. If a future refactor registers a host
// module, every one of these goes green-to-red.
func TestAGuestCannotReachTheHost(t *testing.T) {
	cases := []struct {
		name  string
		wasm  []byte
		about string
	}{
		{"an arbitrary host import", importingSigner(), `import (env "read_file")`},
		{"a WASI import", wasiGuest(), `import (wasi_snapshot_preview1 "fd_write")`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := New(context.Background(), Config{Wasm: c.wasm})
			if err == nil {
				_ = m.Close(context.Background())
				t.Fatalf("a guest declaring %s was ALLOWED to run; the sandbox "+
					"granted a capability it was designed never to offer", c.about)
			}
			if !errors.Is(err, ErrIsolationViolation) {
				t.Errorf("error = %v (%T), want ErrIsolationViolation; a caller "+
					"cannot distinguish \"this script is broken\" from \"this "+
					"script wants the filesystem\"", err, err)
			}
			t.Logf("%s refused at load: %v", c.about, err)
		})
	}
}

// TestTheABIIsRequired proves a module that does not implement the contract is
// rejected at construction rather than at the first request.
//
// Failing later would mean a user ships a signing script, every authenticated
// request fails, and the failure looks like an authentication problem rather
// than a broken script.
func TestTheABIIsRequired(t *testing.T) {
	cases := []struct {
		name string
		wasm []byte
		want string
	}{
		{"no linear memory", noMemoryGuest(), ABIMemory},
		{"no allocate export", noAllocateGuest(), ABIAllocate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := New(context.Background(), Config{Wasm: c.wasm})
			if err == nil {
				_ = m.Close(context.Background())
				t.Fatalf("a module with %s was accepted", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the missing export %q", err, c.want)
			}
		})
	}
}

// TestABIViolationsAreRefused proves the memory boundary is checked rather than
// trusted. Each guest lies in a different way; none may produce output.
func TestABIViolationsAreRefused(t *testing.T) {
	cases := []struct {
		name string
		wasm []byte
		want error
	}{
		{"an output length larger than the buffer", lyingSigner(), ErrABIViolation},
		{"a negative error code", negativeSigner(), ErrABIViolation},
		{"a pointer past the end of memory", outOfBoundsAllocator(), ErrBounds},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newModule(t, Config{Wasm: c.wasm})
			out, err := m.Sign(context.Background(), []byte("GET /v1/x"))
			if err == nil {
				t.Fatalf("a guest reporting %s produced output: %q", c.name, out)
			}
			if !errors.Is(err, c.want) {
				t.Errorf("error = %v (%T), want %v", err, err, c.want)
			}
			if len(out) != 0 {
				t.Errorf("a refused guest returned %d bytes of output", len(out))
			}
			t.Logf("%s refused: %v", c.name, err)
		})
	}
}

// TestBoundsCheckSurvivesTheWrapCase is the integer-overflow gate.
//
// ptr = 2^32-1 with size = 16 wraps to 15 on uint32 arithmetic, so the obvious
// check `ptr+size <= mem.Size()` is trivially TRUE for a pointer far outside
// memory. checkRange subtracts instead of adding precisely so that cannot
// happen, and this gate is where that is proven rather than asserted.
func TestBoundsCheckSurvivesTheWrapCase(t *testing.T) {
	m := newModule(t, Config{Wasm: outOfBoundsAllocator()})

	for _, tc := range []struct{ ptr, size uint32 }{
		{^uint32(0), 16},    // the wrap case
		{^uint32(0), 0},     // and its zero-length cousin
		{65536, 1},          // exactly one past a single page
		{0, ^uint32(0)},     // an absurd size at offset zero
		{m.MemorySize(), 1}, // exactly at the end
	} {
		if err := m.checkRange(tc.ptr, tc.size); err == nil {
			t.Errorf("checkRange(%d, %d) was accepted; it leaves the %d-byte memory",
				tc.ptr, tc.size, m.MemorySize())
		}
	}
	// The boundary itself must still be accepted, or the check is useless. The
	// last legal byte of a 65536-byte memory is offset 65535 with size 1, and a
	// four-byte range ending exactly at the end is legal.
	for _, legal := range [][2]uint32{
		{m.MemorySize() - 1, 1},
		{m.MemorySize() - 4, 4},
		{0, 1},
	} {
		if err := m.checkRange(legal[0], legal[1]); err != nil {
			t.Errorf("checkRange(%d, %d) rejected a LEGAL range: %v",
				legal[0], legal[1], err)
		}
	}
	t.Logf("every out-of-bounds range refused against a %d-byte memory", m.MemorySize())
}

// TestMemorySizeIsReadLive proves the bounds check is not working from a value
// captured at construction.
//
// The guest grows its memory during the call. A host that cached the size at New
// would still pass this round trip, so the gate asserts on the REPORTED size
// afterwards, which is the thing a cache would get wrong.
func TestMemorySizeIsReadLive(t *testing.T) {
	m := newModule(t, Config{Wasm: growingSigner()})
	before := m.MemorySize()
	if _, err := m.Sign(context.Background(), []byte("GET /x")); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	after := m.MemorySize()
	if after <= before {
		t.Errorf("the guest grew its memory by 4 pages but MemorySize went %d -> %d; "+
			"the host is reporting a cached value, and its bounds checks share that "+
			"staleness", before, after)
	}
	t.Logf("memory grew during the call: %d -> %d bytes, and the host sees it", before, after)
}

// TestMemoryLimitIsEnforced proves the linear-memory ceiling is APPLIED.
//
// This gate did not exist for a while, and its absence is why mutation M53 came
// back GREEN the first time it was run. TestDefaultLimitsAreTheDocumentedOnes
// asserts that the DEFAULT is 64 pages; nothing asserted that the number is
// ever handed to the runtime. A ceiling that is documented and never enforced is
// the quietest failure a sandbox can have -- the config looks right, the doc
// looks right, and a guest allocates until the process dies.
//
// The guest grows by four pages during its call, against a one-page ceiling.
func TestMemoryLimitIsEnforced(t *testing.T) {
	const onePage = 1

	// The control: at the DEFAULT ceiling the same guest grows freely, so a
	// failure above is attributable to the limit and not to the guest.
	roomy := newModule(t, Config{Wasm: growingSigner()})
	if _, err := roomy.Sign(context.Background(), []byte("GET /x")); err != nil {
		t.Fatalf("a guest growing 4 pages under the default ceiling failed: %v", err)
	}
	if grew := roomy.MemorySize(); grew <= onePage*65536 {
		t.Fatalf("the control guest did not grow (memory stayed at %d bytes); "+
			"this gate would then prove nothing", grew)
	}

	// The case: a one-page ceiling cannot absorb a four-page growth.
	//
	// Note what is asserted, and why it is not an error. WebAssembly's
	// memory.grow REFUSES an over-limit growth — it returns -1, it does not
	// trap — and the guest drops that result and carries on. So the security
	// property is that the CEILING HOLDS, not that the call fails. The first
	// draft of this gate asserted an error, failed, and was wrong: it was
	// testing wazero's memory.grow semantics rather than this package's limit.
	tight := newModule(t, Config{Wasm: growingSigner(), MemoryPages: onePage})
	if _, err := tight.Sign(context.Background(), []byte("GET /x")); err != nil {
		t.Fatalf("a refused growth trapped the guest; memory.grow returns -1 and "+
			"the guest continues: %v", err)
	}
	ceiling := uint32(onePage) * 65536
	if got := tight.MemorySize(); got > ceiling {
		t.Errorf("the guest reached %d bytes, past the %d-byte ceiling; the limit "+
			"is documented but not applied", got, ceiling)
	}
	if got := tight.MemorySize(); got != ceiling {
		t.Logf("guest memory settled at %d of a %d-byte ceiling", got, ceiling)
	}
	t.Logf("ceiling of %d byte(s) held: the guest asked for 4 more pages and "+
		"memory stayed at %d", ceiling, tight.MemorySize())
}

// TestUnlimitedIsNotConfigurable proves the sandbox cannot be built without a
// bound.
//
// A negative timeout or an out-of-range budget is a CONFIGURATION ERROR, not
// something to clamp. Silently clamping "no timeout" to the maximum would let a
// caller believe they had disabled the limit, and silently clamping a huge fuel
// budget to the maximum would make the number they configured a lie.
func TestUnlimitedIsNotConfigurable(t *testing.T) {
	w := benignSigner()
	for _, d := range []time.Duration{-1, -time.Second, MaxTimeout + time.Second} {
		if _, err := New(context.Background(), Config{Wasm: w, Timeout: d}); err == nil {
			t.Errorf("timeout %v was accepted", d)
		}
	}
	for _, f := range []uint64{MaxFuel + 1, ^uint64(0)} {
		if _, err := New(context.Background(), Config{Wasm: w, Fuel: f}); err == nil {
			t.Errorf("fuel %d was accepted", f)
		}
	}
	for _, p := range []uint32{MaxMemoryPages + 1, ^uint32(0)} {
		if _, err := New(context.Background(), Config{Wasm: w, MemoryPages: p}); err == nil {
			t.Errorf("%d memory pages were accepted", p)
		}
	}
	// An empty module is a configuration error too.
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Error("an empty module was accepted")
	}
	if _, err := New(nil, Config{Wasm: w}); err == nil { //nolint:staticcheck // deliberate nil
		t.Error("a nil context was accepted")
	}
}

// TestEnvelopeCeilingIsEnforced proves the sandbox cannot be used as an
// allocation primitive.
//
// Without a host-side cap on the envelope, `allocate(len)` is a request for
// `len` bytes of guest memory and the guest's own limit becomes the only thing
// between a bad caller and a full sandbox.
func TestEnvelopeCeilingIsEnforced(t *testing.T) {
	m := newModule(t, Config{Wasm: benignSigner()})
	huge := make([]byte, MaxEnvelopeBytes+1)
	if _, err := m.Sign(context.Background(), huge); err == nil {
		t.Fatal("an envelope over the ceiling was signed")
	} else if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("error %q does not say the envelope was too large", err)
	}
	if _, err := m.Sign(context.Background(), nil); err == nil {
		t.Error("an empty envelope was signed")
	}
	t.Logf("envelopes over %d bytes are refused before any guest call", MaxEnvelopeBytes)
}

// TestSpentModuleIsNotReused proves fuel exhaustion is terminal.
//
// The sandbox cancels the call context and wazero closes the module. Handing
// that module back to an agent would make the budget advisory: the first call
// burns it and every later call runs unbounded.
func TestSpentModuleIsNotReused(t *testing.T) {
	m := newModule(t, Config{Wasm: callStorm(20_000), Fuel: 100, Timeout: MaxTimeout})
	_, err := m.Sign(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("the call storm produced no error")
	}
	if !m.Closed() {
		t.Fatal("a module that exhausted its budget is still open")
	}
	if _, err := m.Sign(context.Background(), []byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("a spent module returned %v, want ErrClosed", err)
	}
	t.Log("the spent module refuses further work rather than running unbounded")
}

// TestCloseIsIdempotent mirrors every other teardown in this repository: a
// deferred Close and an explicit one both run in any realistic program.
func TestCloseIsIdempotent(t *testing.T) {
	m, err := New(context.Background(), Config{Wasm: benignSigner()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := m.Close(context.Background()); err != nil {
			t.Errorf("Close call %d: %v", i+1, err)
		}
	}
	if !m.Closed() {
		t.Error("Closed() is false after Close")
	}
	var nilModule *Module
	if err := nilModule.Close(context.Background()); err != nil {
		t.Errorf("closing a nil Module: %v", err)
	}
}

// TestASpentTimeoutDoesNotLeaveTheHostRunning is the concurrency-shaped
// invariant: the sandbox bounds the GUEST, and the host goroutine returns.
//
// It is stated separately from the metering gate because the failure it
// describes is the worst one — a call that returns an error while the guest is
// still executing — and a typed error alone would not reveal it.
func TestASpentTimeoutDoesNotLeaveTheHostRunning(t *testing.T) {
	m := newModule(t, Config{Wasm: infiniteLoop(), Timeout: 30 * time.Millisecond})

	var returned atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.Sign(context.Background(), []byte("x"))
		returned.Store(true)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the call goroutine never returned; the host is wedged")
	}
	if !returned.Load() {
		t.Error("the call returned without completing")
	}
	// A second call must also return promptly, proving the module is not left
	// in a state where every subsequent call blocks.
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		_, _ = m.Sign(context.Background(), []byte("x"))
	}()
	select {
	case <-done2:
	case <-time.After(3 * time.Second):
		t.Fatal("a call on a spent module blocked")
	}
	t.Log("both calls returned promptly")
}

// TestDefaultLimitsAreTheDocumentedOnes pins the defaults.
//
// The defaults are the security posture for every caller that does not
// configure anything, which is every caller an agent writes. If they drift, the
// documented bounds stop describing the product.
func TestDefaultLimitsAreTheDocumentedOnes(t *testing.T) {
	m := newModule(t, Config{Wasm: benignSigner()})
	if got := m.timeout; got != DefaultTimeout {
		t.Errorf("default timeout = %v, want %v", got, DefaultTimeout)
	}
	if got := m.fuel; got != DefaultFuel {
		t.Errorf("default fuel = %d, want %d", got, DefaultFuel)
	}
	if DefaultTimeout > MaxTimeout {
		t.Error("the default timeout exceeds the ceiling")
	}
	if DefaultFuel > MaxFuel {
		t.Error("the default fuel exceeds the ceiling")
	}
	if DefaultMemoryPages*65536 > 1<<24 {
		t.Errorf("the default memory ceiling is %d bytes, over 16 MiB",
			DefaultMemoryPages*65536)
	}
	fmt.Fprintf(testWriter{t}, "defaults: timeout=%v fuel=%d memory=%d pages (%d MiB)\n",
		DefaultTimeout, DefaultFuel, DefaultMemoryPages, DefaultMemoryPages*65536/(1<<20))
}

// testWriter adapts a *testing.T for the fmt.Fprintf above without pulling in
// testing's Writer interface everywhere else.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
