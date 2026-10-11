# DESIGN_SYSTEM.md — Pulse Terminal UI Design System

| Field | Value |
|---|---|
| Document | TUI Design System |
| Product | Pulse |
| Version | 1.0.0-draft |
| Status | Authoritative |
| Governed by | `AGENTS.md` |
| Last updated | 2026-10-11 |

---

## 1. Design Principles

1. **60 FPS or it doesn't ship.** Every `View` must return within the frame budget
   under load (streaming 50MB+ responses included).
2. **Terminal-first, not terminal-tolerated.** Layouts are designed for a grid of
   character cells — no "web app squeezed into 80 columns."
3. **Keyboard is the API.** The mouse is optional garnish; every action has a
   discoverable keybinding.
4. **Elm purity.** All state transitions flow through `Update`; `View` is a pure
   function of `Model`. No I/O, no time calls, no globals in the render path.
5. **Degrade gracefully, never ugly-broken.** Truecolor → ANSI 16 → monochrome all
   remain fully usable, legible interfaces.

## 2. Stack & Elm Architecture Boundaries

| Layer | Technology | Rules |
|---|---|---|
| Runtime | `bubbletea` | `Init/Msg/Update/View/Command` only |
| Styling | `lipgloss` | All color/border/spacing; no raw ANSI strings |
| Components | `bubbles` (viewport, list, textinput, spinner, progress) | Compose, don't fork |
| Forms | `huh` | Modal input flows (new request, env var entry) |

### 2.1 Forbidden in the render path

- Network I/O, disk reads, blocking syscalls (AGENTS.md §3.2 — absolute).
- Time calls inside `View` (inject `now` via messages only).
- Side effects in `Update` — spawn `tea.Cmd`; the runtime owns the goroutines.
- Direct `os.Stdout` writes of any kind.

### 2.2 Message taxonomy

| Class | Examples | Handling |
|---|---|---|
| `tea.KeyMsg` | user input | Keymap dispatch (see §5) |
| `EngineMsg` | `ChunkMsg`, `RunStartedMsg`, `RunDoneMsg` | Append to model, mark dirty |
| `FrameMsg` | `tickMsg` (16ms) | Coalesce pending redraws; drop stale frames |
| `CmdMsg` | `CopyDoneMsg`, `SavedMsg` | Status-bar toast |

Frame coalescing: at most one `View` per frame tick, regardless of message arrival
rate. Backlog is dropped, never buffered unboundedly.

## 3. Layout System (Split-Pane, Flexbox-Style)

### 3.1 Frame anatomy

```
┌─ header: pulse ─ collection.dotfiles ─ env: local ─────────── conn: ● ─┐
├─ tree pane ─┬─ request pane (tab: Editor)  or  response pane (tab: Body)─┤
│  j/k nav    │                                                          │
│  / filter   │                                                          │
│  Enter open │                                                          │
├─ status bar ─┴─ mode: NORMAL  run: GET /users 200 128ms 4.2KB ──────────┤
└─ footer: ? help   ^P find   ^S send   ^E env   q quit ──────────────────┘
```

| Region | Content | Resize behavior |
|---|---|---|
| Header (1 row) | Brand, active collection, environment, connection indicator | Fixed width, truncates left-to-right |
| Tree pane (left, 24 cols) | Virtualized collection/request tree | Collapses to 0 below 100 cols |
| Main pane (flex) | Tabs: `Editor` \| `Body` \| `Headers` \| `Timing` \| `Stream` | Takes all remaining width |
| Status bar (1 row) | Mode, last run summary | Fixed |
| Footer (1 row) | Contextual keybinding hints | Fixed |

### 3.2 Responsive breakpoints

| Width | Layout |
|---|---|
| ≥ 120 cols | Tree \| Request \| Response (three columns) |
| 100–119 cols | Tree \| tabbed main (request or response) |
| 80–99 cols | Tabbed main only; tree reachable via Ctrl+P |
| < 80 cols | Refuse to render half-broken UI: stderr warning, exit 2 |

Height: minimum 24 rows. The response pane is a virtualized viewport (bubbles
`viewport`/`list` with pagination) — 10,000 rows cost O(visible rows).

## 4. Color System

### 4.1 Semantic tokens (dark-first; light theme is a token remap)

| Token | Dark (Truecolor) | Role |
|---|---|---|
| `bg.base` | `#0E1116` | App background |
| `bg.surface` | `#161B22` | Panes, cards |
| `border.default` | `#30363D` | Pane borders |
| `text.primary` | `#C9D1D9` | Body text |
| `text.muted` | `#8B949E` | Hints, metadata |
| `accent.primary` | `#58A6FF` | Selection, focus, links |
| `method.get` | `#3FB950` | GET |
| `method.post` | `#D29922` | POST |
| `method.put` | `#58A6FF` | PUT |
| `method.patch` | `#BC8CFF` | PATCH |
| `method.delete` | `#F85149` | DELETE, errors |
| `status.2xx` | `#3FB950` | Success |
| `status.3xx` | `#39C5CF` | Redirects |
| `status.4xx` | `#D29922` | Client errors |
| `status.5xx` | `#F85149` | Server errors |
| `diff.add` | `#238636` | NDJSON/JSON row highlight (arrived) |
| `diff.del` | `#DA3633` | Removed/failed rows |

### 4.2 Degradation ladder (mandatory)

Lip Gloss renders through a detected color profile; the palette MUST degrade
deterministically. No hex color may be used unconditionally.

| Profile | Detection | Mapping |
|---|---|---|
| Truecolor | `$COLORTERM=truecolor` | Exact hex values above |
| ANSI 256 | `TERM` contains `256color` | Nearest xterm-256 from a fixed lookup table (shipped in repo) |
| ANSI 16 | Standard `TERM` | Named-color equivalents (semantic contrast preserved: methods stay distinguishable, 2xx/4xx/5xx stay distinguishable) |
| Monochrome | `TERM=dumb` / no profile | Distinguish by attribute only: bold/underline/reverse; all semantic meaning must survive without hue |

The 256-color and 16-color lookup tables live in `internal/ui/theme/palette.go` and
are snapshot-tested: rendering a fixture screen at each profile is a golden test.

## 5. Keybinding Philosophy (Vim Motions)

Uniformity law: **the same key does the same thing in every pane.** If a binding must
differ in an insert context (huh forms, textinput), the form captures keys and the
footer shows the context.

### 5.1 Global

| Key | Action |
|---|---|
| `j` / `k` | Down / up (item, line, chunk) |
| `g` / `G` | Top / bottom |
| `Ctrl+d` / `Ctrl+u` | Half-page down / up |
| `Ctrl+f` / `Ctrl+b` | Full page down / up |
| `/` | Search / filter (current pane) |
| `n` / `N` | Next / previous match |
| `Ctrl+P` | Fuzzy find: files, requests, environments, commands |
| `Ctrl+S` | Send current request |
| `Ctrl+X` | Cancel in-flight request |
| `Ctrl+E` | Switch environment |
| `Tab` / `Shift+Tab` | Cycle pane focus |
| `h` / `l` | Collapse / expand pane or tree node |
| `Enter` | Activate (open request, follow link) |
| `y` | Yank (copy) selection |
| `?` | Help overlay (full keymap reference) |
| `q` / `Esc` | Quit context / back (never inside an active textinput) |
| `Ctrl+C` | Hard quit (always available) |

### 5.2 Pane-local

| Pane | Additional |
|---|---|
| Tree | `o`/`Enter` open, `r` rename, `d` delete (confirm via huh), `a` new request |
| Editor | `i` insert mode (huh/textinput), `v` visual select, `:` env var jump, `w` save |
| Response body | `=` pretty / raw toggle, `s` wrap toggle, `{` prev / `}` next NDJSON row |
| Headers | `y` yank value (redacted-safe copy) |
| Stream | `Space` pause/resume tail |

Every keybinding is registered in one central keymap struct
(`internal/ui/keys`) — help overlay, `?`, and footer hints render from it. A new
binding without a keymap entry fails review.

## 6. Typography & Glyphs

| Concern | Standard |
|---|---|
| Font expectation | Nerd Fonts v3 (indicated in docs; auto-detection is impossible in-terminal) |
| Glyph mode | `pulse.toml` → `ui.glyphs = "nerdfont" \| "ascii"`; default `nerdfont` |
| Nerd Font set | `` (folder), `` (file), (branch) git, (lock) secret, (check) pass, (x) fail, (chevron) expand/fold, powerline separators `▏▎▍▌▋▊▉` for progress bars |
| ASCII fallback | `[D]` folder, `[-]` file, `[g]` git, `[*]` secret, `OK`/`XX`, `+`/`-`, block characters `#`/`.` |
| Borders | Rounded (`lipgloss.RoundedBorder`) by default; `Normal`/`Thick` for emphasis |
| Text wrap | Soft-wrap at pane width; long URLs hard-truncate with `…` |

Golden tests render every component in both glyph modes; a tofu-box glyph in the
default profile is a test failure.

## 7. Motion & Feedback

| Pattern | Spec |
|---|---|
| Streaming indicator | Spinner (dots, 10 FPS throttle — spinners animate on a separate cadence, never the full frame budget of the data panes) |
| Progress | Powerline bar for known `Content-Length`; byte counter for chunked |
| Toasts | Status-bar only; 2s fade; no modal interruptions |
| Motion opt-out | `ui.motion = false` (respects `REDUCED_MOTION`-style settings and `--no-motion` flag) disables spinner/animation entirely |

## 8. Accessibility Constraints

- Never convey state by color alone — pair with glyph/text (`[200] OK`, `[404] XX`).
- Monochrome profile must preserve all semantic distinctions (§4.2).
- Focus indicators: accent border + cursor glyph; always exactly one focused pane.
- No flashing >3 Hz anywhere.

## 9. UI Testing Requirements

| Test | Gate |
|---|---|
| Golden snapshot per breakpoint (120/100/80/79 cols) | Byte-identical against fixtures |
| Golden snapshot per color profile (truecolor/256/16/mono) | Semantic contrast assertions |
| Frame budget under 50MB stream load | p99 < 16ms |
| Keymap completeness | Every binding present in help overlay source |
| Glyph mode parity | ASCII fixtures exist for every Nerdfont fixture |

---

*UI pattern changes require updating this document in the same commit (AGENTS.md §4.6).*
