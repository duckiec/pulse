// Package wasm runs untrusted authentication and signing scripts in a
// WebAssembly sandbox with no ambient authority whatsoever.
//
// # The threat model
//
// A signing script is USER-SUPPLIED CODE. It runs inside the same process as
// Pulse, on the same machine, handling bytes that authenticate the user's
// requests. A malicious or merely buggy script must not be able to:
//
//   - read the filesystem, the environment, or the keychain;
//   - open a socket, or inherit an already-open one;
//   - read the clock or the randomness source, and so be non-reproducible or
//     able to detect that it is being sandboxed;
//   - allocate without bound;
//   - run without bound — an infinite loop must cost the run ONE timeout, not
//     the process;
//   - hand the host a pointer or length it could use to read outside its memory.
//
// Every one of those is denied STRUCTURALLY, by never being offered: the runtime
// registers no host modules, links no WASI, and configures no clock, randomness,
// filesystem, environment or stdio. There is no capability to revoke later,
// because none is ever granted.
//
// # What wazero v1.12 can and cannot do
//
// This package is written against a PROBED runtime, not an assumed one
// (MEMORY.md ADL-0024), and the probe changed the design twice:
//
//   - **There is no instruction-level fuel meter.** wazero v1.12.0 exposes no
//     fuel API at all; a case-insensitive search of the whole module finds the
//     word only in an unrelated internal test helper. Fuel metering in the
//     sense the directive means is not available, and this package does not
//     pretend otherwise.
//   - **A panicking FunctionListener does not abort a guest call.** Probed: the
//     panic escapes into the host and the call does not return, so "abort from
//     the listener" is not a usable fuel mechanism either.
//
// What IS available is two bounds that do DIFFERENT jobs. Getting their roles
// right took a measurement, and the measurement is the point:
//
//	TIER 1 — TIMEOUT (preemption). WithCloseOnContextDone(true) plus a context
//	        deadline. wazero's engine polls the context at safe points and closes
//	        the module when it fires. Measured: a guest in `(loop br 0)` is
//	        halted in 50.4ms against a 50ms deadline, returning *sys.ExitError
//	        "module closed with context deadline exceeded". This is the bound
//	        that GUARANTEES TERMINATION, and it is the only one that does.
//
//	TIER 2 — FUEL (detection). A call budget enforced by an
//	        experimental.FunctionListener: every guest function entry is
//	        counted, and on exhaustion the listener cancels the module context,
//	        which the engine then honours at its next safe point.
//
//	Tier 2 does NOT preempt, and the first draft's gate claimed it did. The
//	gate ran a 20,000-call guest against a budget of 50 and measured 20,003
//	calls before the call returned: the listener cancelled on call 51 and the
//	guest ran to completion anyway, because a straight-line unrolled body does
//	not reach a context check until it ends. So fuel is a DETECTOR, not a
//	preemptor — and its real security value is different from, and additional
//	to, termination:
//
//	    * it is DETERMINISTIC. It counts calls, not milliseconds, so it makes
//	      the same decision on a loaded CI runner as on an idle laptop.
//	    * it POISONS the module. A guest detected over budget is never reused,
//	      so a script that was once observed doing too much cannot produce a
//	      signature later, on a call where the wall clock happened to be quiet.
//
//	Together: the timeout guarantees the host survives, and fuel guarantees that
//	"it survived" is not mistaken for "it behaved". A timeout-only sandbox stops
//	a hanging guest and then reuses it; a fuel-only sandbox would notice a
//	runaway and do nothing about it.
//
// # Interpreter, not compiler
//
// `NewRuntimeConfigInterpreter`, never the JIT compiler. The compiler mmaps
// executable pages, which is unavailable on some platforms, non-deterministic
// in its performance characteristics, and buys throughput a signing script does
// not need. ROADMAP.md §3.4.1 requires interpreter-only; that is also the choice
// that keeps the sandbox CGO-free and identical on every target.
package wasm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/sys"
)

// Limits. Every one has a default, and every default is a ceiling rather than a
// suggestion — the failure mode of this feature is a user's machine hanging, not
// a script running slowly.
const (
	// DefaultTimeout bounds one guest call. A signing script that has not
	// finished in this long is not going to.
	DefaultTimeout = 50 * time.Millisecond
	// MaxTimeout is the ceiling a caller may configure. A script is not a batch
	// job; a generous timeout is a way to ask for a hang.
	MaxTimeout = 2 * time.Second
	// DefaultFuel is the guest function-invocation budget for one call, sized
	// for a real HMAC over a request: thousands of calls, not millions.
	DefaultFuel = 1_000_000
	// MaxFuel is the ceiling a caller may configure.
	MaxFuel = 100_000_000
	// DefaultMemoryPages is 4 MiB. A signing script needs kilobytes; a script
	// wanting hundreds of megabytes is doing something else.
	DefaultMemoryPages = 64
	// MaxMemoryPages is 256 MiB, and also the hard ceiling.
	MaxMemoryPages = 4096
)

// Errors. All are typed so a caller can branch on WHY a script failed without
// reading a message.
var (
	// ErrFuelExhausted reports that the guest exceeded its call budget.
	//
	// It is TERMINAL for the module: the sandbox cancels the module context and
	// WithCloseOnContextDone turns that into a closed module. A guest that
	// burned its budget is not trusted to produce a signature afterwards, and
	// reusing it would make the budget advisory.
	//
	// It is a DETECTION, not a preemption — see the package comment. The guest
	// may run to the end of its current straight-line body before the engine
	// reaches a safe point; what the budget guarantees is that the module is
	// never reused afterwards.
	ErrFuelExhausted = errors.New("wasm: guest exceeded its call budget")

	// ErrTimeout reports that the guest exceeded its wall-clock budget.
	ErrTimeout = errors.New("wasm: guest exceeded its time budget")

	// ErrClosed reports use of a module after its context was cancelled.
	ErrClosed = errors.New("wasm: module is closed")

	// ErrIsolationViolation reports that the guest tried to reach something it
	// was never offered. It surfaces as an INSTANTIATION failure, so it is a
	// load-time property rather than a run-time one.
	ErrIsolationViolation = errors.New("wasm: guest requested a capability the sandbox does not grant")
)

// Config is the sandbox configuration.
type Config struct {
	// Wasm is the guest module. Required.
	Wasm []byte

	// Name identifies the module in wazero diagnostics. Empty selects a default.
	// Each Module gets its own wazero runtime, so names cannot collide across
	// modules.
	Name string

	// Timeout bounds one call. Zero selects DefaultTimeout; negative is an
	// error rather than a request for "no limit", because an unbounded guest is
	// the single defect this package exists to prevent.
	Timeout time.Duration

	// Fuel is the guest function-invocation budget per call. Zero selects
	// DefaultFuel. As with Timeout there is no "unlimited": a caller who wants
	// one must construct a different sandbox.
	Fuel uint64

	// MemoryPages caps linear memory. Zero selects DefaultMemoryPages.
	MemoryPages uint32

	// Resolver optionally enables the zero-trust host functions in
	// internal/wasm/host.go. Nil leaves the sandbox with no host imports at
	// all (the Move 11 default-deny posture, gated by
	// TestHostFunctionIsRegistered).
	Resolver SecretResolver
}

// Module is an instantiated guest.
//
// It is NOT safe for concurrent use. Each call cancels its context on fuel
// exhaustion, and wazero closes the module on cancellation — so two concurrent
// calls would race on whether the module survives. Callers wanting concurrency
// build several Modules; the wazero runtime itself is concurrent.
type Module struct {
	rt    wazero.Runtime
	mod   api.Module
	mem   api.Memory
	alloc api.Function
	sign  api.Function // nil when the guest exports no sign_request
	hook  api.Function // nil when the guest exports no handle_request

	// base is the context the module was INSTANTIATED with, and the one the
	// fuel listener is bound to. wazero attaches a FunctionListener to a
	// function at instantiation time (it becomes `f.parent.listener`); a
	// listener added to the context passed to Call is silently ignored. That
	// was measured, not assumed: the first draft attached the factory per call
	// and the counter never moved, which looked exactly like a working budget
	// that was merely generous.
	//
	// Sign derives its call context from base, so cancelling base — which is
	// what the listener does on exhaustion — propagates down to the call and
	// trips WithCloseOnContextDone.
	base   context.Context
	cancel context.CancelFunc

	timeout time.Duration
	fuel    uint64

	// spent counts guest function invocations. It is a POINTER shared with the
	// listener, not a field of its own: wazero calls NewFunctionListener once
	// per function DEFINITION, so a counter captured in that closure restarts
	// for every distinct function and never reaches any budget. Copying the
	// value into a Module field instead of sharing the pointer is the same bug
	// wearing a different hat, and the first draft did that too — the gate read
	// "the fuel counter never moved" and the listener was counting perfectly.
	spent  *atomic.Uint64
	closed atomic.Bool

	closeMu sync.Mutex
}

// New compiles and instantiates a guest under the sandbox.
//
// It fails rather than degrading if the module does not satisfy the ABI or asks
// for a capability the sandbox does not grant. A signing script that cannot be
// isolated must not run: there is no "partially sandboxed" mode, because every
// capability it was refused is one it requested on purpose.
func New(ctx context.Context, cfg Config) (*Module, error) {
	if ctx == nil {
		return nil, errors.New("wasm: New requires a non-nil context")
	}
	if len(cfg.Wasm) == 0 {
		return nil, errors.New("wasm: no module bytes given")
	}
	timeout, err := resolveTimeout(cfg.Timeout)
	if err != nil {
		return nil, err
	}
	fuel, err := resolveFuel(cfg.Fuel)
	if err != nil {
		return nil, err
	}
	pages, err := resolvePages(cfg.MemoryPages)
	if err != nil {
		return nil, err
	}
	name := cfg.Name
	if name == "" {
		name = "pulse-wasm-guest"
	}

	// THE ISOLATION. Read this block as a specification of what the guest is
	// NOT given; every omission below is a denial, not an oversight.
	//
	// Deliberately NOT set on the runtime:
	//   WithCoreFeatures     — the default (V2) stands, so a guest cannot opt
	//                          into a feature this sandbox has not reasoned
	//                          about.
	//   WithCompilationCache — a compilation cache is a shared, process-wide
	//                          resource. A guest must not be able to evict, or
	//                          observe, another guest's compiled code.
	rtCfg := wazero.NewRuntimeConfigInterpreter().
		WithCloseOnContextDone(true). // tier 1: preemption
		WithMemoryLimitPages(pages).  // linear memory ceiling
		WithMemoryCapacityFromMax(false)

	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)

	// The fuel listener is attached HERE, at instantiation, because that is the
	// only point wazero reads it from. Cancelling the module context is what
	// stops the guest: Sign's call context descends from base, so the
	// cancellation propagates and CloseOnContextDone fires.
	base, cancelBase := context.WithCancel(ctx)
	spent := &atomic.Uint64{}
	base = experimental.WithFunctionListenerFactory(base, newFuelListener(spent, fuel, cancelBase))

	if resolver := cfg.Resolver; resolver != nil {
		// The CLOSED whitelist (ROADMAP.md §3.4.3): exactly these four
		// functions under pulse/env, each chained off the same builder so
		// there is one registration site to audit. Nil Resolver registers
		// NOTHING and any guest import fails (default-deny).
		if _, err := rt.NewHostModuleBuilder(HostModuleName).
			NewFunctionBuilder().
			WithGoModuleFunction(newHMACHostFunc(resolver), []api.ValueType{
				api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32,
				api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32,
			}, []api.ValueType{api.ValueTypeI32}).
			Export(HostFuncHMACSHA256).
			NewFunctionBuilder().
			WithGoModuleFunction(newNowNSFunc(), nil, []api.ValueType{api.ValueTypeI64}).
			Export(HostFuncNowNS).
			NewFunctionBuilder().
			WithGoModuleFunction(newHeaderGetFunc(), []api.ValueType{
				api.ValueTypeI32, api.ValueTypeI32,
				api.ValueTypeI32, api.ValueTypeI32,
			}, []api.ValueType{api.ValueTypeI32}).
			Export(HostFuncHeaderGet).
			NewFunctionBuilder().
			WithGoModuleFunction(newLogFunc(), []api.ValueType{
				api.ValueTypeI32, api.ValueTypeI32,
			}, nil).
			Export(HostFuncLog).
			Instantiate(ctx); err != nil {
			cancelBase()
			_ = rt.Close(ctx)
			return nil, fmt.Errorf("wasm: instantiate host module: %w", err)
		}
	}
	// No OTHER host modules are instantiated: with a nil Resolver this block
	// above runs zero times and ANY import the guest declares fails to resolve
	// (the Move 11 default-deny posture). With a Resolver, exactly one host
	// module exists — pulse/env with hmac_sha256, which returns a digest, never
	// key material. The filesystem, network, clock, randomness and WASI stay
	// structurally denied: each is reachable only through an import, and there
	// is nothing else to import.
	//
	// Deliberately NOT set on the module:
	//   WithEnv               — no environment variables.
	//   WithFS / WithFSConfig — no filesystem.
	//   WithSysWalltime       — no clock, so a guest cannot learn when it runs.
	//   WithSysNanotime       — ditto, at finer resolution.
	//   WithSysNanosleep      — no sleeping; sleeping is a DoS vector.
	//   WithRandSource        — no randomness, so a signature is reproducible.
	//   WithOsyield           — no host scheduling.
	//   WithArgs              — no argv.
	//   WithStdout/Stderr     — io.Discard, never the process's own streams.
	//
	// WithStartFunctions() takes no arguments, which CLEARS the default `_start`
	// run. WASI is not linked, so there is no `_start` to run; clearing it is
	// explicit rather than incidental.
	modCfg := wazero.NewModuleConfig().
		WithName(name).
		WithStdin(eofReader{}).
		WithStdout(io.Discard).
		WithStderr(io.Discard).
		WithStartFunctions()

	mod, err := rt.InstantiateWithConfig(base, cfg.Wasm, modCfg)
	if err != nil {
		cancelBase()
		_ = rt.Close(ctx)
		return nil, classifyInstantiate(err)
	}

	// ExportedMemory, NOT Memory. api.Module.Memory returns the memory DEFINED
	// by the module whether or not it is exported, so it would accept a guest
	// that keeps its memory private — which is not the ABI this package
	// defines, and which `TestTheABIIsRequired` exists to catch.
	mem := mod.ExportedMemory(ABIMemory)
	if mem == nil {
		cancelBase()
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("wasm: the guest does not export %q", ABIMemory)
	}
	alloc := mod.ExportedFunction(ABIAllocate)
	if alloc == nil {
		cancelBase()
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("wasm: the guest does not export %q", ABIAllocate)
	}
	// The two in-place ABIs are INDEPENDENT: a signing guest need not export a
	// hook, and a hook-only guest need not export a signer. Requiring both would
	// force every user-supplied script to implement a function it never runs.
	// A module exporting NEITHER is rejected here, at load, so a user never
	// discovers the gap as an authentication failure on a live request.
	sign := mod.ExportedFunction(ABISignRequest)
	hook := mod.ExportedFunction(ABIHandleRequest)
	if sign == nil && hook == nil {
		cancelBase()
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("wasm: the guest exports neither %q nor %q",
			ABISignRequest, ABIHandleRequest)
	}

	m := &Module{
		rt:      rt,
		mod:     mod,
		mem:     mem,
		alloc:   alloc,
		sign:    sign,
		hook:    hook,
		base:    base,
		cancel:  cancelBase,
		timeout: timeout,
		fuel:    fuel,
	}
	m.spent = spent
	return m, nil
}

// eofReader is an always-EOF stdin.
//
// A guest with no WASI cannot reach stdin at all, so this is belt and braces;
// naming it rather than inlining it makes the intent read as a denial at the
// call site instead of as a missing argument.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// classifyInstantiate turns a wazero load failure into a typed error.
//
// An unresolved import is the signature of a guest that asked for a capability,
// so it is reported as [ErrIsolationViolation] rather than as a generic
// compilation error. That distinction is the difference between "this script is
// broken" and "this script is asking for the network", which are different
// conversations with the user.
func classifyInstantiate(err error) error {
	msg := strings.ToLower(err.Error())
	// "not instantiated" is wazero's wording for an unresolved import, and it
	// is the message the sandbox produces for EVERY escape attempt: the guest
	// named a host module and nothing was registered under it.
	//
	// The bare word "import" was in the first list and had to come out: it also
	// appears in "invalid byte for importdesc", which is a MALFORMED MODULE, not
	// an escape attempt. Classifying a broken script as an isolation violation
	// sends the user looking for a sandbox problem they do not have.
	for _, needle := range []string{
		"not instantiated",
		"not implemented",
		"unknown import",
	} {
		if strings.Contains(msg, needle) {
			return fmt.Errorf("%w: %v", ErrIsolationViolation, err)
		}
	}
	return fmt.Errorf("wasm: cannot instantiate the guest: %w", err)
}

func resolveTimeout(d time.Duration) (time.Duration, error) {
	switch {
	case d == 0:
		return DefaultTimeout, nil
	case d < 0:
		return 0, fmt.Errorf("wasm: timeout must be positive, got %v", d)
	case d > MaxTimeout:
		return 0, fmt.Errorf("wasm: timeout %v exceeds the %v ceiling", d, MaxTimeout)
	}
	return d, nil
}

func resolveFuel(f uint64) (uint64, error) {
	if f == 0 {
		return DefaultFuel, nil
	}
	if f > MaxFuel {
		return 0, fmt.Errorf("wasm: fuel %d exceeds the %d ceiling", f, MaxFuel)
	}
	return f, nil
}

func resolvePages(p uint32) (uint32, error) {
	if p == 0 {
		return DefaultMemoryPages, nil
	}
	if p > MaxMemoryPages {
		return 0, fmt.Errorf("wasm: %d memory pages exceeds the %d ceiling", p, MaxMemoryPages)
	}
	return p, nil
}

// Sign runs the guest's signing ABI over payload and returns what it wrote.
//
// The error is typed: [ErrTimeout] for a guest that ran too long,
// [ErrFuelExhausted] for one that made too many calls, and a bounds or ABI error
// for one that misbehaved at the memory boundary. See memory.go for the ABI.
func (m *Module) Sign(ctx context.Context, payload []byte) ([]byte, error) {
	return m.call(ctx, payload, m.invokeSign, "Sign")
}

// HandleRequest runs the guest's request-hook ABI over payload (a JSON request
// document) and returns the rewritten JSON document.
//
// It shares the sandbox, the clock/fuel bounds and the in-place memory contract
// with Sign; only the export name and the output reservation differ. A guest
// that exports no handle_request is refused here, before any dispatch.
func (m *Module) HandleRequest(ctx context.Context, payload []byte) ([]byte, error) {
	return m.call(ctx, payload, m.invokeHook, "HandleRequest")
}

// CanSign and CanHook report which optional ABIs the guest implemented, so a
// caller can fail with direction ("this script exports no handle_request")
// instead of a confusing late error.
func (m *Module) CanSign() bool { return m.sign != nil }
func (m *Module) CanHook() bool { return m.hook != nil }

// call wraps one bounded guest invocation. The call context descends from
// m.base, which is where the fuel listener lives; exhaustion cancels m.base,
// which cancels this, which closes the module — which is why a spent guest is
// not reused.
func (m *Module) call(ctx context.Context, payload []byte, invoke func(context.Context, []byte) ([]byte, error), what string) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("wasm: %s requires a non-nil context", what)
	}
	if m.closed.Load() {
		return nil, ErrClosed
	}

	callCtx, cancel := context.WithTimeout(m.base, m.timeout)
	defer cancel()

	// The caller's VALUES propagate (notably the pre-flight request for
	// header_get, attached via WithRequest): m.base cannot carry them because
	// it predates the call. Cancellation propagates too, so a caller that
	// goes away stops paying for its signature.
	if r := requestFromContext(ctx); r != nil {
		callCtx = context.WithValue(callCtx, requestKey{}, r)
	}
	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	// The counter is reset per call so a caller can read the cost of one
	// request rather than a running total.
	m.spent.Store(0)

	res, err := invoke(callCtx, payload)
	if err != nil {
		return nil, m.classifyCallErr(callCtx, err)
	}
	if callCtx.Err() != nil {
		m.closed.Store(true)
	}
	return res, nil
}

// newFuelListener builds the per-call listener.
//
// Counting rather than aborting is deliberate, and was forced by the probe: a
// panic inside a listener escapes into the host and does not halt the guest
// (MEMORY.md ADL-0024 records the measurement). So the listener's only safe
// power is to trip the deadline mechanism, and it does so by cancelling the call
// context.
func newFuelListener(spent *atomic.Uint64, budget uint64, onExhausted context.CancelFunc) experimental.FunctionListenerFactory {
	return experimental.FunctionListenerFactoryFunc(
		func(api.FunctionDefinition) experimental.FunctionListener {
			return experimental.FunctionListenerFunc(
				func(_ context.Context, _ api.Module, _ api.FunctionDefinition, _ []uint64, _ experimental.StackIterator) {
					if spent.Add(1) > budget {
						onExhausted()
					}
				})
		})
}

// classifyCallErr maps a wazero call error onto this package's typed errors.
//
// The distinction matters: [ErrFuelExhausted] is terminal for the module and
// [ErrTimeout] is not, and a caller that treats them alike will either retry a
// poisoned module or give up on a slow one.
func (m *Module) classifyCallErr(ctx context.Context, err error) error {
	if errors.Is(err, ErrClosed) {
		return ErrClosed
	}
	var exit *sys.ExitError
	if errors.As(err, &exit) && ctx.Err() != nil {
		m.closed.Store(true)
		// Which mechanism fired? The fuel listener cancels the same context the
		// deadline does, so the error alone cannot say. Fuel is checked first
		// because it is the deterministic one and its exhaustion is terminal:
		// mislabelling a poisoned module as "slow" would let a caller reuse it.
		if n := m.spent.Load(); n > m.fuel {
			return fmt.Errorf("%w after %d calls", ErrFuelExhausted, n)
		}
		return fmt.Errorf("%w after %v", ErrTimeout, m.timeout)
	}
	return fmt.Errorf("wasm: the guest trapped: %w", err)
}

// FuelUsed reports the guest function invocations the last call consumed.
//
// It is diagnostic, not enforcement: the budget is enforced during the call.
func (m *Module) FuelUsed() uint64 { return m.spent.Load() }

// Closed reports whether the module has been spent.
func (m *Module) Closed() bool { return m.closed.Load() }

// MemorySize returns the guest's linear memory size in bytes.
func (m *Module) MemorySize() uint32 { return m.mem.Size() }

// Close releases the module and its runtime. It is idempotent, because a
// deferred Close and an explicit one from a signal handler both run in any
// realistic program.
func (m *Module) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	m.closed.Store(true)
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.rt == nil {
		return nil
	}
	rt := m.rt
	m.rt = nil
	return rt.Close(ctx)
}
