# CLAUDE.md — Agent entry point for warp-go

This file gives Claude Code (and other agent-driven tooling) repo-specific conventions.
The engineering skills under the `mattpocock-skills` plugin look here for their configuration.

## Repo at a glance

- Go module `warp` — a Cloudflare WARP WireGuard/MASQUE tunnel client (fork of `badafans/warp-go`).
- Entry point: `main.go`. Supporting packages: `tunnel/`, `registration/`, `scanner/`, plus recent `frontproxy/` work.
- Domain reference (reverse-engineering notes): `docs/warp-masque-reverse-engineering.md`.
- Containerised: `Dockerfile` (multi-stage) + `.dockerignore`.

## Agent skills

### Issue tracker

Issues for this repo live as GitHub issues (uses the `gh` CLI). See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical labels — `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context — one `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
