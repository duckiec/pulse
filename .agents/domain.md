# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase.

## Before exploring, read these

- **`GLOSSARY.md`** at the repo root.
- **`MEMORY.md`** at the repo root — this repo's **decision ledger**. Every architectural
  decision is an `ADL-nnnn` entry under `### ADL-nnnn — <title>`. Read the entries that
  touch the area you are about to work in.

There is **no `docs/adr/` directory, and there will not be one.** `MEMORY.md` is already
the append-only decision ledger (AGENTS.md §4.6), it already holds 26 entries, and it is
already the thing CI checks. A parallel ADR directory would give this repository two
sources of truth for the same decisions with no rule for which wins — the exact failure
mode AGENTS.md §4.5 exists to prevent, applied to architecture instead of tests.

If you find yourself about to create `docs/adr/`, you are about to fork the ledger. Write
an ADL entry in `MEMORY.md` instead.

If `GLOSSARY.md` does not yet exist, **proceed silently**. Don't flag its absence; don't
suggest creating it upfront. The `/domain-modeling` skill (reached via `/grill-with-docs`
and `/improve-codebase-architecture`) creates it lazily when terms actually get resolved.

## File structure

Single-context repo:

```
/
├── GLOSSARY.md          ← vocabulary
├── MEMORY.md            ← decisions (ADL-nnnn entries), append-only
└── internal/
```

The seven Essential Vibe Coding Files (`AGENTS.md`, `PRD.md`, `ROADMAP.md`,
`ARCHITECTURE.md`, `DESIGN_SYSTEM.md`, `TASKS.md`, `MEMORY.md`) are the **complete**
documentation surface. `.gitignore` enforces it with a default-deny `*.md` plus anchored
whitelist entries. Adding an eighth tracked markdown file is an amendment to AGENTS.md
§4.6 and requires a MEMORY.md ADL entry recording it — see ADL-0026 for the one narrow
exception (`.agents/` config and `GLOSSARY.md`).

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a
hypothesis, a test name), use the term as defined in `GLOSSARY.md`. Don't drift to
synonyms the glossary explicitly avoids.

If the concept you need isn't in the glossary yet, that's a signal: either you're inventing
language the project doesn't use (reconsider) or there's a real gap (note it for
`/domain-modeling`).

## Flag ADR conflicts

This repo calls them **ADL entries**, not ADRs. If your output contradicts an existing
entry, surface it explicitly rather than silently overriding:

> _Contradicts ADL-0025 (AES-256-GCM, random nonce per seal), but worth reopening
> because…_

And the corollary matters just as much: **contradicting an ADL entry requires a new ADL
entry.** Amending `MEMORY.md` is append-only. You do not edit ADL-0025 to reverse it; you
append ADL-0027 saying what changed and why. The old entry stays as the record of what was
believed at the time — a ledger that rewrites itself is a changelog, and a changelog that
rewrites itself is a fiction.

## Where the domain docs are NOT

Three things that look like domain documentation but are deliberately not, and should not
be "cleaned up" into it:

- **`MEMORY.md`'s Refs lines.** They point at files; they do not duplicate them. Do not
  restate a decision's content in a second file.
- **`ARCHITECTURE.md` §16+.** Those are descriptions of the system as built, not
  decisions about the system. ADL entries record *why*; ARCHITECTURE records *what*.
- **Commit messages.** They are the third ledger and they are Git-native, which is exactly
  why ADL-0015 makes `origin/main` canonical. Don't copy decisions out of history into
  markdown; reference the commit.