# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go CLI that migrates a self-hosted GitLab to GitLab SaaS. It copies repository
content (all branches/tags) over the **plain git protocol** (`git clone --mirror`
→ `git push --mirror`) and handles structure + metadata over the **GitLab REST
API v4**. The two-channel design is deliberate: git transport makes the copy
version-independent, the API is only used where it is stable across versions.

## Commands

```sh
go build ./...                 # build all packages
go build -o gitlab-copy-tool . # build the CLI binary
go vet ./...
gofmt -l .                     # CI fails if this prints anything; use `gofmt -w .`
go test ./...                  # all tests
go test ./internal/rewrite/ -run TestRunDivergentPathMapping -v   # a single test
```

- **Always `gofmt -w .` before committing** — CI (`.github/workflows/ci.yml`) has a
  gofmt gate plus build/vet/test, then cross-compiles into `dist/`.
- The `gittransport` tests shell out to the real `git` binary against local bare
  repos — `git` must be installed; no network/GitLab needed.
- Run modes: `./gitlab-copy-tool` (interactive TUI), `--dry-run` (TUI, resolves the
  plan without pushing), `sessions list|rm`, `run --session <name> [--dry-run] [--path-map f]`.

## Non-obvious conventions

- **GitLab IDs are `int64`** everywhere (client-go v1.46 changed these from `int`;
  a mismatch is the most common compile break when touching API types).
- The GitLab client is `gitlab.com/gitlab-org/api/client-go` (package `gitlab`),
  the successor of the deprecated `xanzy/go-gitlab`.
- **Failure policy:** only ensuring the target group/project and the repo mirror
  are "hard" steps that can mark a project `failed`. URL rewrite and metadata copy
  are failsafe — on error they only add a warning and the project still succeeds.
  Keep new optional steps failsafe.
- Sessions live under `os.UserConfigDir()/gitlab-copy-tool/sessions/*.json` (mode
  `0600`). The migration **path map is per-session** (`Session.PathMap`), not a
  global file. Tokens may be `${ENV_VAR}` references, resolved via
  `config.ResolveToken` at runtime.

## Architecture (how the packages fit together)

Data flow of a run: **discovery → mapping → ResolveTargets → Plan → Engine.Run →
per project: ensure namespace/project → mirror (guard) → URL-rewrite commit →
metadata copy.**

- `internal/gitlabapi` — API wrapper. `SourceTree()` builds the group→project tree
  (personal-namespace projects are intentionally excluded). **`ResolveTargets()` is
  the single source of truth** for turning per-node target *assignments* into an
  effective target namespace per project: a group-level assignment cascades to
  descendants while preserving substructure; the nearest assignment wins. It is
  shared by both the TUI and the CLI — change target-resolution logic only here.
  `EnsureGroupPath`/`EnsureProject` replicate source **name, path slug and
  visibility** onto target groups/projects (via `BuildGroupHints`/`SetGroupHints`),
  upgrading existing groups' visibility on re-runs.
- `internal/gittransport` — the version-independent copy. `Mirror()` clones the
  source mirror and pushes it; the **existing-target guard** fetches target refs
  and uses `git merge-base --is-ancestor` to decide overwrite vs. skip-with-reason
  (or force-overwrite-with-warning). `worktree.go` does the shallow checkout +
  commit + push used for the rewrite step.
- `internal/rewrite` — regex-based, boundary-safe host+path rewrite in every
  `composer.json` and every root file. Exact path-map hits take precedence over
  the generic host-swap + account-prefix fallback; original URL form (https/scp/
  ssh) is preserved.
- `internal/migrate` — `Engine` orchestrates one run. `Plan` carries `Roots` (for
  group hints), `ExtraPaths` (the session path map), and per-item `Force`.
  `pathMappings()` merges session + current-run mappings (current wins);
  `RecordPathMappings()` feeds successful results back into the session map.
- `internal/tui` — Bubble Tea state machine split into `model.go` / `update.go` /
  `view.go`; screens are `session → connect → discover → map → run/dryRun/done`.
  Run events stream from the engine over a channel consumed as `tea.Msg`.
- `internal/config` — session + path-map persistence and token resolution.
- `main.go` — cobra CLI; the root command launches the TUI, `run` executes a saved
  session non-interactively (and reuses `gitlabapi.ResolveTargets`).

## Constraints worth remembering

- Target namespaces are groups with ≥ Maintainer access; personal namespaces are
  excluded on both sides.
- GitLab rule: project/subgroup visibility ≤ parent. A public repo under a private
  target account is impossible until the account is made visible — the tool warns
  rather than failing.
- The URL rewrite can only fix the *path* of repos known in the current session
  (migrated earlier or in the same run); otherwise only host + account prefix is
  applied.
