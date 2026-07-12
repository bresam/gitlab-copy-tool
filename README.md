# gitlab-copy-tool

[![CI](https://github.com/bresam/gitlab-copy-tool/actions/workflows/ci.yml/badge.svg)](https://github.com/bresam/gitlab-copy-tool/actions/workflows/ci.yml)
[![Version](https://img.shields.io/github/v/tag/bresam/gitlab-copy-tool?sort=semver&label=version)](https://github.com/bresam/gitlab-copy-tool/tags)

**Migrate a self-hosted GitLab to GitLab SaaS (gitlab.com) — version-independent, with an interactive mapping UI in your terminal.**

`gitlab-copy-tool` copies your repositories **completely, with all branches and
tags**, recreates the group/folder structure on the target, and optionally
transfers issues, merge requests, CI/CD variables and settings. The core runs
over the plain Git protocol, so it largely **doesn't matter how far apart the
GitLab versions** of source and target are.

```
        self-hosted GitLab                                GitLab SaaS (gitlab.com)
   ┌───────────────────────────┐                     ┌───────────────────────────────┐
   │ 📁 tools           (priv) │      git mirror     │ 📁 example-org       (public) │
   │   ├─ deployment ──────────┼─── all branches ───▶│   └─ 📁 tools          (priv) │
   │   └─ 📁 ci                │        + tags       │       ├─ deployment           │
   │       └─ runner           │                     │       └─ 📁 ci                │
   └───────────────────────────┘   REST API v4       │           └─ runner           │
        (source)                (groups, issues,     └───────────────────────────────┘
                                 MRs, CI vars, …)          (target, freely mapped)
```

Repository content (branches/tags) travels via `git clone --mirror` →
`git push --mirror`; structure & metadata via the stable GitLab REST API v4.

> **The source GitLab is only read, never modified.** The tool clones from the
> source and creates/updates on the target only — it never touches source repos,
> groups or settings. Archiving or deleting the old repositories after a
> successful migration is a **manual step** you perform yourself.

## Download

Prebuilt binaries for the **latest release** (no runtime dependency):

- 🐧 **Linux (x86-64)** — [gitlab-copy-tool-linux-amd64](https://github.com/bresam/gitlab-copy-tool/releases/latest/download/gitlab-copy-tool-linux-amd64)
- 🐧 **Linux (ARM64)** — [gitlab-copy-tool-linux-arm64](https://github.com/bresam/gitlab-copy-tool/releases/latest/download/gitlab-copy-tool-linux-arm64)
- 🍎 **macOS (Apple Silicon)** — [gitlab-copy-tool-darwin-arm64](https://github.com/bresam/gitlab-copy-tool/releases/latest/download/gitlab-copy-tool-darwin-arm64)
- 🍎 **macOS (Intel)** — [gitlab-copy-tool-darwin-amd64](https://github.com/bresam/gitlab-copy-tool/releases/latest/download/gitlab-copy-tool-darwin-amd64)
- 🪟 **Windows (x86-64)** — [gitlab-copy-tool-windows-amd64.exe](https://github.com/bresam/gitlab-copy-tool/releases/latest/download/gitlab-copy-tool-windows-amd64.exe)

On Linux/macOS make it executable (`chmod +x gitlab-copy-tool-*`). macOS may
quarantine an unsigned binary — clear it with
`xattr -d com.apple.quarantine gitlab-copy-tool-darwin-arm64`. Or
[build from source](#installation).

---

## What the tool shows in the terminal

The central screen is the **mapping**: on the left the source tree with
checkboxes and tree lines, on the right the resolved target per repo. Both the
**target namespace** and the **optional steps** can be set per repo **or at the
group level** (inherited by the substructure, nearest override wins). The
options block shows the settings for the highlighted node; `*` marks an option
set explicitly here, dim ones are inherited.

```
 Mapping   (source → target namespace, group target is inherited)

  [~] 📁 tools                          ⇒ example-org/… (inherited)
▸ │  ├─ [x] deployment   → example-org/tools/deployment        ✓ transferred
  │  └─ 📁 ci
  │     └─ [x] runner    → example-org/tools/ci/runner  [force]
  └─ 📁 legacy
     └─ [ ] old-tool

 Options for deployment (set with 1-6; * = set here, else inherited):
 1 [x] Issues/MRs   2 [x] CI-Vars   3 [x] Settings
 4 [x] URL-Rewrite  5 [x] Releases  6 [x] Container-Registry*
```

> Note: the screens shown here are translated for the docs; the shipped UI
> labels are currently in German. The behaviour is identical. An English UI is on
> the roadmap.

---

## Features

### Repositories (the core)
- **All branches and tags** are copied 1:1 (`git clone --mirror` →
  `git push --mirror`) — version-independent, no API feature dependency.
- **Existing-target guard:** if the target repo already exists, its refs are
  compared. If the target has *nothing newer*, it is fully overwritten. If it has
  newer or divergent commits/branches, the repo is **skipped with a reason**
  (data-loss protection) — overridable per repo with `f` (force).
- **Selectable transport:** `auto` (SSH via your agent first, HTTPS token
  fallback), `ssh` or `https` — per instance.

### Structure
- **Group-tree discovery:** groups → subgroups → projects as a tree.
- **Free mapping:** target namespace per repo, or at the **group level** with
  cascade — a group target is inherited as a prefix by all subgroups/repos and
  **preserves their substructure**; individual entries stay overridable.
- **Group creation:** missing target (sub)groups are created automatically —
  with the **exact name and path slug of the source** (a group named "Public"
  with path "pub" stays that way).
- **Visibility replication:** private/public of source groups and repos is
  replicated on the target and reconciled on **every** run (groups are only
  raised, never lowered). GitLab SaaS has no `internal` level, so source
  **`internal` maps to `private`** (never public). Where GitLab forbids a level
  (e.g. public under a private target account), a warning is emitted instead of
  a hard failure.

### Metadata (optional, failsafe)
These steps are toggleable **per repo or per group** (cascading to the
substructure, nearest override wins); if one fails you only get a **warning** —
the repo still counts as successfully migrated. Repos that actually have
container images get the **container registry** step auto-enabled on discovery:
- **Issues + open merge requests + labels + milestones**
- **Releases** (name, description, tag, asset links, milestones — the tags
  already exist from the mirror; source archives are regenerated by GitLab)
- **CI/CD variables**
- **Project settings** (description)
- **Container registry** — copies all images + tags (incl. multi-arch) registry-
  to-registry **in pure Go** (via [go-containerregistry](https://github.com/google/go-containerregistry));
  no external tool or Docker daemon required. Off by default; needs tokens with
  registry access. Skipped with a warning if the registry is disabled.

### composer.json / URL rewrite
After the push, the old GitLab **host** in references is replaced — in every
`composer.json` (at any depth) and in all files in the repo root, as **one**
extra commit on the default branch:
- **All URL forms:** `https://`, `http://`, scp-like `git@host:…` and
  `ssh://git@host/…` (the form is preserved).
- **Path carried over:** for migrated repos the full new path is set (even when
  the namespace differs due to the mapping), not just the host.
- **Account prefix:** references to not-yet-migrated repos get the host swapped
  and the target account (first segment of the target namespace) prepended.
- **Per-session path map:** every successful run remembers `old → new`, so later
  runs also correctly rewrite references to previously migrated repos.

### Convenience
- **Incremental re-runs:** an already-transferred repo is **skipped** (`unchanged`)
  unless its config changed (target, options, force) **or** the source has new
  commits/branches. Each repo is cloned into a temp dir, mirror-pushed, and the
  temp dir is removed immediately — at most ~one repo is on disk at a time.
- **Sessions:** connections, selection, mapping and options are saved and offered
  for reuse on startup.
- **Dry run:** shows the resolved plan without pushing anything.
- **Non-interactive:** saved sessions can be run scriptably (CI).

---

## How it works

| Aspect | Path | Why |
|--------|------|-----|
| Repo content | `git` mirror clone/push | plain Git protocol → version-independent, copies all refs |
| Structure & metadata | GitLab REST API v4 | very stable across versions |
| Fault tolerance | only group/project/mirror are "hard" | everything else warns and continues |

---

## Installation

Requires **Go 1.26+** and an installed **`git`**.

```sh
git clone https://github.com/bresam/gitlab-copy-tool.git
cd gitlab-copy-tool
go build -o gitlab-copy-tool .
```

This produces the `./gitlab-copy-tool` binary (no runtime dependency).

### Prerequisites
- A **personal access token** (scope `api`) for source and target each.
- For the SSH transport: matching SSH keys in your local SSH agent.
- For the optional **container registry** copy: tokens with registry access
  (the `api` scope covers this). No external tool needed — the copy is pure Go.

---

## Quick start

```sh
./gitlab-copy-tool
```

Flow in the UI:

1. Choose a **session** or "＋ New session".
2. **Connections**: source URL + token, target URL + token, transport
   (`auto`/`ssh`/`https`). Test the connection with `ctrl+s`.
3. **Discovery**: source structure + target namespaces are loaded.
4. **Mapping**: select repos/groups with `space`, set the target with `←/→`
   (cascades on a group), toggle options `1`–`4`.
5. **Start** with `ctrl+s` → live progress, then a summary at the end.

> Tip: start with **`--dry-run`** first (see below) and review the plan before
> anything is actually pushed.

### Keeping tokens safe
Tokens may be entered as an **environment reference**, e.g. `${SRC_TOKEN}`. Then
only the reference is stored in the session file, and the value is read from the
environment at runtime:

```sh
export SRC_TOKEN=glpat-…   TGT_TOKEN=glpat-…
./gitlab-copy-tool          # enter ${SRC_TOKEN} / ${TGT_TOKEN} in the form
```

---

## Keyboard shortcuts

**Session picker:** `↑/↓` select · `enter` open · `c` clear state · `d` delete · `q` quit

**Mapping:**

| Key | Action |
|-----|--------|
| `↑/↓` | Navigate |
| `space` | (De)select item (a group toggles all its children) |
| `←/→` | Cycle the target namespace — on a group it cascades, on a repo it applies only there |
| `f` | Toggle force-overwrite for this repo |
| `a` / `N` | Select all / none |
| `1`–`6` | Cycle the highlighted node's option (inherit → on → off) — Issues/MRs, CI vars, Settings, URL rewrite, Releases, Container registry. On a group it cascades to the substructure |
| `ctrl+s` | Start the migration (or show the plan in dry-run) |
| `esc` | Back |

`q` quits from any screen — except the connection form (where you type values
that may contain `q`; there `ctrl+c` quits).

---

## Dry run

Shows the resolved plan (source → target per repo) but pushes nothing and changes
nothing:

```sh
./gitlab-copy-tool --dry-run           # interactive: pick/create a session, then plan
./gitlab-copy-tool run --dry-run       # same
./gitlab-copy-tool run --session NAME --dry-run   # non-interactive
```

In dry-run mode, `c` clears a session only **temporarily** (nothing is written to
disk, shown as `[temporarily cleared]`).

---

## Sessions & non-interactive use

Sessions live under `~/.config/gitlab-copy-tool/sessions/<name>.json` (file mode
`0600`) and contain connections, selection, mapping, options and the path map —
but **not** plaintext tokens if you use `${ENV}` references.

```sh
./gitlab-copy-tool sessions list
./gitlab-copy-tool sessions rm "<name>"

# run a saved session (e.g. in CI):
./gitlab-copy-tool run --session "<name>"
./gitlab-copy-tool run --session "<name>" --path-map extra.json
```

In the session picker, `c` clears the **state** (selection, target assignments,
force flags, path map) — URLs, tokens and options are kept. `d` deletes the
session entirely.

---

## Existing-target guard (data-loss protection)

Before overwriting an already existing target repo, the tool compares the refs:

- Target **has nothing newer** → fully overwrite (`git push --mirror`).
- Target **has newer/divergent** commits or its own branches → **skipped with a
  reason** (in the log and the summary).
- With `f` (force) per repo it is overwritten anyway — then reported as a
  **warning** stating what was overwritten.

---

## Known limitations

- **Group projects** are migrated; projects in the personal namespace (source and
  target) are excluded.
- Target namespaces = groups with at least Maintainer access.
- Issue/MR import copies title/description/labels (for MRs also source/target
  branch), but no comments, authors, discussions or numbers (best effort). Only
  **open** MRs are recreated.
- GitLab rule: project/subgroup ≤ parent group. A public repo under a private
  target account is therefore only possible once the account itself is visible
  enough — otherwise a warning.
- When setting the path, the URL rewrite only knows repos migrated **in this
  session** (earlier or in the same run). For a not-yet-migrated repo only the
  host + account prefix applies — migrate the dependency first or in the same run.
- Container registry copy is registry-to-registry in pure Go; it can move a lot
  of data. Package and model registries are **not** copied — they are usually
  rebuilt/republished by CI after migration.

---

## Security

- Session files use mode `0600`. Tokens are stored in plaintext there **unless**
  you use `${ENV_VAR}` references (recommended for shared/committed setups).
- The tool only talks to the two GitLab instances you specify.
- The **source instance is read-only** for the tool — nothing on the source is
  modified. Retiring old repos (archive/delete) is a manual step on your side.

---

## Project layout

```
main.go                 CLI (cobra): interactive, sessions, run
internal/
  config/               session persistence + path map
  gitlabapi/            REST API: discovery, groups/projects, metadata
  gittransport/         git mirror clone/push, existing-target guard
  rewrite/              host/path rewrite in composer.json & root files
  migrate/              orchestration of a run
  tui/                  Bubble Tea UI (screens)
```

---

## License

[MIT](LICENSE).
