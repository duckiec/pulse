# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the actual label strings used in this repo's issue tracker (GitHub Issues — see `issue-tracker.md`).

| Label in mattpocock/skills | Label in our tracker | Meaning                                  |
| -------------------------- | -------------------- | ---------------------------------------- |
| `needs-triage`             | `needs-triage`       | Maintainer needs to evaluate this issue  |
| `needs-info`               | `needs-info`         | Waiting on reporter for more information |
| `ready-for-agent`          | `ready-for-agent`    | Fully specified, ready for an AFK agent  |
| `ready-for-human`          | `ready-for-human`    | Requires human implementation            |
| `wontfix`                  | `wontfix`            | Will not be actioned                     |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label string from this table.

Edit the "Label in our tracker" column to match whatever vocabulary you actually use.

## A note on `ready-for-agent` in this repo

"Ready for an AFK agent" is a **narrower** claim here than it is in most repos. AGENTS.md
§4.7 is a hard gate: an agent that cannot answer every box on the Definition of Done is
not done, and §4.2 requires red→green mutation evidence for every fix.

An issue marked `ready-for-agent` in `duckiec/pulse` should therefore already answer:

1. **Which gate proves it.** Not "add tests" — which test, named.
2. **What the exit condition is.** The observable that flips from false to true.
3. **What the mutation is.** Per §4.2, the single-line change that turns the new gate
   red. An issue that names no mutation has not specified the work; it has requested it.

An issue that cannot answer (1)–(3) belongs in `needs-info`, not `ready-for-agent`.