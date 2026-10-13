package mcp

// Redactor scrubs secrets out of anything the bridge reports to an agent.
//
// It is declared HERE, as a one-method interface, rather than taken as a
// `*secrets.Redactor`. The reason that matters is inversion: naming the concrete
// type would make the choice of redactor a property of the PROTOCOL LAYER
// rather than of the application, which is precisely the coupling a composition
// root exists to remove. Declaring the contract instead lets internal/app — the
// one package that legitimately knows about every sibling — decide what is
// installed and prove it satisfies this.
//
// It also keeps `internal/mcp`'s imports to `ast`, `engine` and `fuzz`, per
// AGENTS.md §3.1: no sibling package imports another sibling.
//
// `*secrets.Redactor` satisfies this, and so does any test double. Nil means
// "no value scrubbing configured"; header-name redaction still applies, and the
// difference is a degradation a caller must not mistake for the whole.
type Redactor interface {
	// String returns s with every registered secret replaced.
	String(s string) string
}
