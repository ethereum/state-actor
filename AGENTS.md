# AGENTS.md

state-actor generates client-ready Ethereum databases for geth, reth, besu, and nethermind without going through each client's `init` path.

The agent-facing canonical doc is **[`docs/SKILL.md`](docs/SKILL.md)** — read it first. This file is a pointer.

## Quick pointers

| Topic | Where |
|---|---|
| How to do common tasks (with full recipes) | [`docs/SKILL.md`](docs/SKILL.md) |
| Spec YAML schema reference | [`docs/SPEC.md`](docs/SPEC.md) |
| Client boot recipes (per-client) | [`docs/RUNBOOK.md`](docs/RUNBOOK.md) |
| Internal architecture; per-package `doc.go` | [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) |
| Example specs (with picker) | [`examples/README.md`](examples/README.md) |
| Canonical spec syntax (every feature) | [`examples/full-matrix-spec-feature.yaml`](examples/full-matrix-spec-feature.yaml) |
| Full CLI flag list | `state-actor --help` |

## Three load-bearing flags

`--client` (which client format to write), `--spec` (YAML state declaration), `--target-size` (upper bound on the whole DB). Everything else has a sane default.

## Tool-specific notes

- **Claude Code** also reads [`CLAUDE.md`](CLAUDE.md), which redirects here.
- **Cursor, Codex, Aider, Gemini CLI**: `AGENTS.md` is the open standard you're reading.
