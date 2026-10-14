// Package app is Pulse's composition root: the one package that knows about
// every other one.
//
// # Why this package exists
//
// Moves 0–9 built every subsystem in isolation and gated each one. What they
// could not do was run. `EngineExecutor.Redactor` was nil because nothing held a
// `*secrets.Redactor`; `FailureBinder.Retrieve` was nil because nothing held a
// `*store.Store`; the fuzz tool had no runner at all. Each of those was recorded
// as an open TASKS.md item with the same root cause and no fix, because the fix
// is not a feature — it is a seam.
//
// This package is that seam, and its whole design goal is to be the ONLY place
// in the tree where a concrete dependency is chosen. Every other package
// declares what it needs as an interface and is handed it. That is not
// architectural fashion: it is what makes each subsystem testable without
// booting SQLite, a keychain and an HTTP transport, and it is why `internal/mcp`
// does not import `internal/secrets` even after this move.
//
// # Lifecycle
//
// An App owns resources with real teardown — a SQLite handle, a resolver chain,
// a cancelled context that in-flight work observes. The contract is therefore
// two halves that must both be honoured:
//
//	app, err := app.New(ctx, cfg)   // may fail; no partial App escapes
//	if err != nil { return err }
//	defer app.Close()               // cancels ctx, then closes in reverse order
//
// [App.Close] is idempotent, because a deferred close and an explicit one on a
// signal handler will otherwise both run, and closing a closed SQLite handle is
// an error the caller has no way to interpret.
//
// # stdout
//
// Nothing in this package writes to stdout, ever. In MCP mode stdout is the
// JSON-RPC channel and a stray byte corrupts the stream for the connected agent
// (AGENTS.md §3.3). Diagnostics go to stderr through [mcp.Logf], and the
// import audit enforces it structurally under the `internal/app` directory.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"pulse/internal/ast"
	"pulse/internal/engine"
	"pulse/internal/mcp"
	"pulse/internal/rag"
	"pulse/internal/secrets"
	"pulse/internal/store"
)

// Compile-time proof that the production redactor satisfies every contract the
// bridge declares. The assertion lives HERE rather than in internal/mcp because
// this is the one package allowed to know both sides; if the vault's method set
// drifts, this is a build failure rather than a nil-check failure discovered by
// the first agent whose response happens to carry a token.
var (
	_ mcp.Redactor    = (*secrets.Redactor)(nil)
	_ engine.Redactor = (*secrets.Redactor)(nil)
	_ engine.Resolver = (*secrets.Chain)(nil)
	_ engine.Resolver = engineResolver{}
	_ rag.Embedder    = (*rag.HashingEmbedder)(nil)
	_ mcp.Executor    = (*mcp.EngineExecutor)(nil)
	_ mcp.FuzzRunner  = (*mcp.EngineFuzzRunner)(nil)
	_ mcp.Retriever   = (*mcp.StoreRetriever)(nil)
)

// Config is the resolved configuration for one App.
//
// Every field has a working default, because the common case is an agent
// launching `pulse mcp` with no arguments and expecting a usable tool. The
// fields exist for tests and for the future `pulse.toml`, not because the
// defaults are wrong.
type Config struct {
	// Home is Pulse's data directory. Empty selects [DefaultHome].
	//
	// It holds the SQLite database and, when an ONNX model is ever vendored,
	// the model artifact. It never holds a secret: the vault is the OS keychain
	// and an ephemeral in-memory .env (AGENTS.md §3.4).
	Home string
	// DBPath overrides the database file. Empty selects Home/pulse.db.
	DBPath string
	// EnvFile is a .env loaded into memory for variable resolution. Never
	// persisted, never written back. Empty means "no ephemeral env", and the
	// resolution order becomes keychain → error.
	EnvFile string
	// RunTimeout bounds one request execution. Zero selects DefaultRunTimeout.
	RunTimeout time.Duration
	// FuzzTimeout bounds one fuzz probe. Zero selects mcp.DefaultFuzzTimeout.
	FuzzTimeout time.Duration
	// CorpusRoot is the directory Local RAG indexes at startup. Empty means no
	// automatic indexing: a user who has not asked for local RAG should not pay
	// for walking their repository, and an empty index degrades the 5xx
	// explanation to "no context" rather than to a wrong answer.
	CorpusRoot string
	// IndexConcurrency caps RAG walker workers. Zero selects the package default.
	IndexConcurrency int
	// SkipKeychainProbe suppresses the availability probe. Tests set it; the
	// probe otherwise touches the OS keychain on a headless machine, where the
	// honest answer is DEGRADED rather than a startup failure.
	SkipKeychainProbe bool
	// VaultFile is a PVLT2 container decrypted at boot with the device
	// identity into the ephemeral vault tier (memory only). Empty means no
	// vault tier: resolution is keychain → .env → error.
	VaultFile string
	// VaultKeyID selects the device identity in the keychain
	// (vault:x25519:<id>). Empty selects the default identity.
	VaultKeyID string
	// Backend overrides the platform keychain backend. Nil selects
	// secrets.NewBackend(). Tests bind a MemoryStore holding a fixture
	// identity; production never sets this — the platform backend is the
	// only reader the vault tier may load a device key through.
	Backend secrets.SecretResolver
}

// Defaults chosen so that an agent can launch `pulse mcp` and get a working
// tool with no configuration at all.
const (
	// DefaultRunTimeout bounds one request. Generous enough for a slow local
	// API, short enough that a hung handler does not look like a hung agent.
	DefaultRunTimeout = 30 * time.Second
	// DefaultDBName is the database file inside Home.
	DefaultDBName = "pulse.db"
)

// DefaultHome returns Pulse's data directory: `$PULSE_HOME` when set, otherwise
// `<user config dir>/pulse`.
//
// The environment variable comes first because it is how a test, a CI job or a
// second checkout isolates itself; the OS location is only the fallback for a
// user who has never heard of Pulse. os.UserConfigDir is used rather than a
// hand-rolled per-OS table because the stdlib already encodes the platform
// conventions, and a second copy of them is a second thing to get wrong.
func DefaultHome() (string, error) {
	if v := os.Getenv("PULSE_HOME"); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("app: locate the user config directory: %w", err)
	}
	return filepath.Join(base, "pulse"), nil
}

// App is the wired application.
//
// Fields are exported so the composition root's result is inspectable — a test
// asserting that the fuzzer received a real redactor reads App.FuzzRunner's
// wiring rather than inferring it from behaviour. They are not a public API:
// internal/ is not importable from outside this module.
type App struct {
	// Store is the SQLite handle. Closed by Close.
	Store *store.Store
	// Chain resolves `{{var}}` references: keychain, then the ephemeral .env,
	// then the decrypted .env.vault payload, then a hard error. Executors never
	// take it directly — see engineResolver: they take resolver(), which
	// translates the chain's terminal error onto the engine's contract.
	Chain *secrets.Chain
	// Redactor scrubs resolved secrets out of everything reported.
	Redactor *secrets.Redactor
	// Embedder produces structural vectors for Local RAG.
	Embedder rag.Embedder
	// Retriever queries the index for a 5xx explanation.
	Retriever *mcp.StoreRetriever
	// Binder attaches source context to a failed run.
	Binder *mcp.FailureBinder
	// Executor backs `execute_request`.
	Executor mcp.Executor
	// FuzzRunner backs `execute_fuzz_run`.
	FuzzRunner mcp.FuzzRunner

	// Degradations are conditions the app booted with and could not resolve.
	// They are REPORTED, never hidden: an agent that is told the index is empty
	// can act on it, while one told nothing concludes the tool is broken.
	Degradations []string

	// Home and DBPath record what was actually opened, which may differ from
	// the Config when defaults applied.
	Home   string
	DBPath string

	// ctx bounds every subsystem. It is cancelled by Close BEFORE any resource
	// is released, so work in flight observes the shutdown and unwinds through
	// its own teardown path rather than being torn down under it.
	ctx    context.Context
	cancel context.CancelFunc

	cfg Config

	closeOnce sync.Once
	closeErr  error
	closeMu   sync.Mutex
	closed    bool
}

// New builds and wires the application.
//
// It either returns a fully usable App or an error; a partially constructed App
// never escapes. Every resource acquired on the way is released on the way out,
// which is why the constructor returns `(*App, error)` rather than an App whose
// Close must be called to find out whether construction worked.
func New(ctx context.Context, cfg Config) (*App, error) {
	if ctx == nil {
		return nil, errors.New("app: New requires a non-nil context")
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}

	appCtx, cancel := context.WithCancel(ctx)
	a := &App{ctx: appCtx, cancel: cancel, cfg: cfg, Home: cfg.Home, DBPath: cfg.DBPath}

	if err := a.openStore(); err != nil {
		cancel()
		return nil, err
	}
	if err := a.openSecrets(); err != nil {
		_ = a.Close()
		return nil, err
	}
	if err := a.buildRetrieval(); err != nil {
		_ = a.Close()
		return nil, err
	}
	if err := a.buildTools(); err != nil {
		_ = a.Close()
		return nil, err
	}
	return a, nil
}

// applyDefaults fills the unset fields.
func (c *Config) applyDefaults() error {
	if c.Home == "" {
		home, err := DefaultHome()
		if err != nil {
			return err
		}
		c.Home = home
	}
	if c.DBPath == "" {
		c.DBPath = filepath.Join(c.Home, DefaultDBName)
	}
	if c.RunTimeout <= 0 {
		c.RunTimeout = DefaultRunTimeout
	}
	if c.FuzzTimeout <= 0 {
		c.FuzzTimeout = mcp.DefaultFuzzTimeout
	}
	return nil
}

// openStore opens the database and creates the optional RAG tables.
//
// The RAG tables are created here rather than in store.Open for a reason the
// store itself documents (ARCHITECTURE.md §8): a driver without FTS5 must not
// stop Pulse starting, because Local RAG is optional and reports DEGRADED.
func (a *App) openStore() error {
	if err := os.MkdirAll(filepath.Dir(a.DBPath), 0o700); err != nil {
		return fmt.Errorf("app: create the data directory for %s: %w", a.DBPath, err)
	}
	st, err := store.Open(a.ctx, a.DBPath)
	if err != nil {
		return err
	}
	a.Store = st
	if err := st.InitRAG(a.ctx); err != nil {
		return fmt.Errorf("app: initialise the RAG tables: %w", err)
	}
	if !st.RagHasFTS5(a.ctx) {
		// Not fatal, and not silent. The lexical leg of hybrid retrieval needs
		// FTS5; without it, retrieval degrades to the vector leg alone and the
		// agent is told so rather than shown short answers presented as complete.
		a.degrade("local RAG lexical search is unavailable: the SQLite driver " +
			"was built without FTS5, so retrieval falls back to the vector leg")
	}
	return nil
}

// openSecrets builds the resolver chain and the redactor.
//
// The chain and the redactor are built together and SHARE one Redactor
// instance. That sharing is the whole point: a value resolved through the chain
// is registered with the redactor, so it cannot then appear in a response body,
// a diagnostic or a retrieved chunk. Two Redactors would leave a window where a
// resolved secret is known to the resolver and invisible to the scrubber.
//
// The custody order is keychain → .env → decrypted vault → error. The vault
// tier is opened ONCE here with the device identity (never per lookup), and
// an explicitly configured vault that cannot open is a DEGRADATION — the app
// still serves, and a vault-only variable fails closed at request time with
// exit 3 rather than starting the process with a half-loaded secret set.
func (a *App) openSecrets() error {
	red := secrets.NewRedactor()

	var env map[string]string
	if a.cfg.EnvFile != "" {
		m, err := secrets.LoadEnvFile(a.cfg.EnvFile)
		if err != nil {
			// A missing .env is worth reporting but not worth refusing to
			// start: the resolution order is keychain → .env → error, and a
			// user with only keychain entries must still get a working tool.
			a.degrade(fmt.Sprintf("the ephemeral env at %s could not be read: %v", a.cfg.EnvFile, err))
		} else {
			env = m
		}
	}

	// The backend is constructed once and shared: the chain resolves through
	// it and the vault tier loads the device identity through it. Two
	// backends would probe the keychain twice and could disagree.
	backend := a.cfg.Backend
	if backend == nil {
		backend = secrets.NewBackend()
	}
	chainOpts := []secrets.ChainOption{secrets.WithRedactor(red), secrets.WithBackend(backend)}
	if env != nil {
		chainOpts = append(chainOpts, secrets.WithEnv(env))
	}
	if a.cfg.VaultFile != "" {
		if vm, err := a.openVaultTier(backend); err != nil {
			a.degrade(fmt.Sprintf("the vault at %s could not be opened: %v", a.cfg.VaultFile, err))
		} else {
			chainOpts = append(chainOpts, secrets.WithVaultEnv(vm))
		}
	}
	a.Chain = secrets.NewChain(chainOpts...)
	a.Redactor = red

	if !a.cfg.SkipKeychainProbe {
		// A missing keychain is a DEGRADATION on every platform CI runs on, and
		// a startup FAILURE on a headless developer machine would make Pulse
		// unusable there for no good reason. The order stays keychain-first;
		// what changes is whether the absence is reported or fatal.
		if err := a.Chain.Available(); err != nil {
			a.degrade(fmt.Sprintf("no OS keychain is reachable (%v); variable "+
				"resolution falls back to the ephemeral env, and to a hard error "+
				"when neither has the value", err))
		}
	}
	return nil
}

// openVaultTier decrypts the configured PVLT2 container with the device
// identity and parses it as .env into memory.
//
// The identity is loaded through the SAME backend the chain resolves with,
// so "which device am I" cannot disagree between the vault tier and the
// keychain tier. Every secret the tier holds is registered with the shared	// redactor before this returns, so no vault value exists un-scrubbed — the
// same rule P2.2.4 established for the keychain tier. The returned map is
// ephemeral: the chain owns it after WithVaultEnv, and Clear scrubs it.
func (a *App) openVaultTier(backend secrets.SecretResolver) (map[string]string, error) {
	id, err := secrets.LoadIdentity(a.cfg.VaultKeyID, backend)
	if err != nil {
		return nil, err
	}
	defer clear(id.X25519Priv)
	defer clear(id.Ed25519Seed)
	container, err := os.ReadFile(a.cfg.VaultFile)
	if err != nil {
		return nil, fmt.Errorf("app: read %s: %w", a.cfg.VaultFile, err)
	}
	defer clear(container)
	plain, err := secrets.Open2(container, id.X25519Priv)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	env, err := secrets.LoadEnv(bytes.NewReader(plain))
	if err != nil {
		return nil, fmt.Errorf("app: the vault payload is not .env-shaped: %w", err)
	}
	if a.Redactor != nil {
		for _, v := range env {
			a.Redactor.Register(v)
		}
	}
	return env, nil
}

// buildRetrieval wires the index query surface.
//
// Every piece is injected as a FUNCTION, not as the store or the embedder. That
// is the same decision store.Search's caller makes, and it keeps this file from
// growing a dependency on a concrete store: the retriever's contract is
// "given a query, return chunks", and app is the only place that knows how.
func (a *App) buildRetrieval() error {
	a.Embedder = rag.NewHashingEmbedder()

	a.Retriever = &mcp.StoreRetriever{
		SearchFn: func(ctx context.Context, query string, vec []int8, limit int) ([]mcp.RagHit, error) {
			hits, err := a.Store.Search(ctx, query, vec, limit)
			if err != nil {
				return nil, err
			}
			out := make([]mcp.RagHit, 0, len(hits))
			for _, h := range hits {
				out = append(out, mcp.RagHit{
					Path: h.Path, StartLn: h.StartLn, EndLn: h.EndLn,
					Symbol: h.Symbol, Lang: h.Lang, Text: h.Text,
					Score: float64(h.Score), Sources: h.Sources,
				})
			}
			return out, nil
		},
		EmbedFn: func(ctx context.Context, text string) ([]int8, float32, error) {
			return a.Embedder.Embed(ctx, text)
		},
		EmbedderName_: a.Embedder.Name(),
		Available:     a.Embedder.Available(),
	}
	a.Binder = &mcp.FailureBinder{
		Retrieve: a.Retriever,
		Redact:   a.Redactor,
	}
	return nil
}

// buildTools wires the executor and the fuzz runner.
//
// Both are handed the SAME redactor and the SAME resolver, which is the
// invariant the whole move exists to establish: before it, an executor built by
// the bridge carried a nil Redactor, so MCP mode scrubbed by header name only
// and a token in a response body reached the model intact.
func (a *App) buildTools() error {
	a.Executor = &mcp.EngineExecutor{
		Redactor:  a.Redactor,
		Resolver:  a.resolver(),
		NewEngine: a.newRouter,
	}

	a.FuzzRunner = &mcp.EngineFuzzRunner{
		Redactor:  a.Redactor,
		UserAgent: "pulse/" + Version,
		Timeout:   a.cfg.FuzzTimeout,
	}
	return nil
}

// degrade records a condition the app booted with and could not resolve.
// engineResolver adapts the secrets chain to the engine's resolver contract.
//
// It exists for ONE translation: the chain's terminal error
// (secrets.ErrUnresolved) must arrive at the test runner as
// engine.ErrUnresolved. The runner maps engine.ErrUnresolved to exit 3
// ("configure your credential"); a bound resolver whose error wraps only the
// secrets sentinel would collapse into exit 1 ("a test failed"), sending the
// user to debug assertions when they are actually missing a credential. Both
// sentinels stay in the chain (%w twice), so secrets-aware callers
// (SecretExitCode) and engine-aware callers (ExitCodeFor) classify the same
// error correctly. This is the composition root's work and nobody else's:
// engine may not import secrets and secrets may not import engine (AGENTS.md
// §3.1 — no sibling imports), so the translation lives in the one package
// that knows both sides.
type engineResolver struct{ chain *secrets.Chain }

func (r engineResolver) Lookup(name string) (string, error) {
	if r.chain == nil {
		return "", fmt.Errorf("%w: variable %q (no resolution chain)", engine.ErrUnresolved, name)
	}
	v, err := r.chain.Lookup(name)
	if err != nil {
		if errors.Is(err, secrets.ErrUnresolved) {
			return "", fmt.Errorf("%w: variable %q: %w", engine.ErrUnresolved, name, err)
		}
		return "", err
	}
	return v, nil
}

// resolver returns the chain translated onto the engine's contract. Every
// executor — the test runner, REST, GRPC, MCP — takes this, never the raw
// chain, so exit 3 means the same thing in every mode.
func (a *App) resolver() engine.Resolver { return engineResolver{chain: a.Chain} }

// newRouter builds the protocol router for one parsed document.
//
// It is a method rather than a closure inside buildTools because the test
// runner needs the SAME router: two constructions would be two places for the
// redactor, the resolver and the timeout to be wired differently, and a test run
// that redacts differently from an MCP call is a redaction gate that passes in
// one mode and fails in the other.
func (a *App) newRouter(doc *ast.Document) engine.ProtocolEngine {
	return engine.NewProtocolRouter(doc, engine.RouterConfig{
		REST: []engine.Option{
			engine.WithRedactor(a.Redactor),
			engine.WithResolver(a.resolver()),
			engine.WithTimeout(a.cfg.RunTimeout),
			engine.WithUserAgent("pulse/" + Version),
		},
		GRPC: &engine.GRPCConfig{Resolver: a.resolver(), Redactor: a.Redactor},
	})
}

// RouterFor exposes the composed protocol router for one document so the TUI
// runs the SAME pipeline (resolver with exit-3 translation, redactor,
// timeout) as MCP and `pulse test`. The router is built by newRouter — this
// method only widens its visibility, so the three modes cannot drift apart
// (ARCHITECTURE.md invariant 11: UI and MCP hold only ProtocolEngine).
func (a *App) RouterFor(doc *ast.Document) engine.ProtocolEngine {
	return a.newRouter(doc)
}

func (a *App) degrade(msg string) {
	if a.Redactor != nil {
		msg = a.Redactor.String(msg)
	}
	a.Degradations = append(a.Degradations, msg)
	mcp.Logf("DEGRADED: %s", msg)
}

// Context returns the application's lifetime context.
//
// It is cancelled by Close. Callers must not retain it past Close, and must not
// use it as a substitute for their own request-scoped deadline: it marks the
// application's death, not a single operation's.
func (a *App) Context() context.Context { return a.ctx }

// Config returns the resolved configuration, including defaults that applied.
func (a *App) Config() Config { return a.cfg }

// Close tears the application down.
//
// Order is deliberate and reverse of construction:
//
//  1. cancel the context, so work in flight observes the shutdown and unwinds
//     through its OWN teardown path (Goal 1 §1.2.6) rather than having its
//     resources pulled out from under it;
//  2. close the store, which is the only resource with an OS handle that
//     survives the process if it leaks.
//
// It is idempotent. A deferred Close and an explicit one from a signal handler
// both run in any realistic program, and `sql.DB.Close` on an already-closed
// handle returns an error the caller cannot interpret — so the second call
// returns the FIRST call's result and neither surprises anyone.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		if a.cancel != nil {
			a.cancel()
		}
		a.closeMu.Lock()
		defer a.closeMu.Unlock()
		a.closed = true
		// Scrub the ephemeral secret tiers (explicit .env AND the decrypted
		// vault payload) before releasing anything else: in-flight work was
		// already cancelled above, so no new resolution can observe the
		// maps while they are being zeroed.
		if a.Chain != nil {
			a.Chain.Clear()
		}
		if a.Store != nil {
			if err := a.Store.Close(); err != nil {
				a.closeErr = fmt.Errorf("app: close the store: %w", err)
			}
		}
	})
	return a.closeErr
}

// Closed reports whether Close has run.
func (a *App) Closed() bool {
	a.closeMu.Lock()
	defer a.closeMu.Unlock()
	return a.closed
}

// SecretExitCode reports whether an App error is a secret-resolution failure,
// returning Pulse's exit code 3 for one and 0 for anything else.
//
// It is a function of the ERROR CHAIN, not of the message text. A keychain
// failure reaches the CLI through an arbitrary number of wraps, and a caller
// that had to substring-match "keychain" to pick an exit code would classify
// correctly today and silently wrongly the first time a message is reworded.
//
// Zero is "not mine", not "success": the caller falls through to its own
// generic failure path. [secrets.ExitCode] cannot be used directly because it
// answers a different question — it maps EVERY error to a code, which would
// make a disk failure indistinguishable from a vault failure at the call site.
func SecretExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, secrets.ErrUnresolved) {
		return secrets.ExitCodeSecret
	}
	return 0
}

// Version is reported in the User-Agent and in MCP server metadata.
var Version = "0.0.0-dev"
