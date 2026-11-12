# gh compact

**Fold too-frequent GitHub releases into tidy ones, and turn failing checks green — across all your repos, in one command.**

- `gh compact` folds noisy releases into tidy ones.
- `gh compact checks` finds failing GitHub checks, tells you why, fixes what it can and re-runs them.

Shipped `v0.1.10` through `v0.1.17` in one afternoon? `gh compact` finds runs of
releases that came out too close together and folds each run into its newest
release. That release keeps its tag, assets and "Latest" badge; its notes gain a
summary of everything it absorbed plus the full notes of each older release. The
older releases are then deleted. Private repos work too.

```text
◆ gh compact  fold noisy releases into tidy ones
  signed in as you

◆ you/pairdesk  8 releases → 2
  ├─ v0.1.17 ⟵ v0.1.16 v0.1.15 v0.1.14  spans 57m · drops 12 old assets (32.7 MiB)
  └─ v0.1.13 ⟵ v0.1.12 v0.1.11 v0.1.10  spans 51m · drops 12 old assets (32.7 MiB)

◆ you/wifishare  3 releases → 1
  └─ v1.1.1 ⟵ v1.1.0 v1.0.0  spans 20.8h

╭──────────────────────────────────────────────────────────────╮
│  Plan: compact 2 repos · remove 8 releases · tags kept       │
│  50 repos scanned                                            │
╰──────────────────────────────────────────────────────────────╯
? Compact now? [y]es / [n]o / [p]ick per repo
```

## Install

You need the [GitHub CLI](https://cli.github.com). Then:

```bash
gh extension install Kumarsujay03/gh-compact
```

That's it. `gh compact` uses your `gh` login; if you aren't signed in yet it starts
`gh auth login` for you. Upgrade with `gh extension upgrade compact`.

Without `gh`: `go install github.com/Kumarsujay03/gh-compact@latest` and set
`GH_TOKEN` to a token with the `repo` scope.

## Use

```bash
gh compact                    # scan every repo you own, show the plan, ask, compact
gh compact --dry-run          # only show the plan
gh compact pairdesk -p        # one repo, and preview the merged notes
gh compact you/a other/b      # just these repos
gh compact --org my-team      # your repos plus an organisation's
gh compact --yes              # no questions (scripts, CI)
gh compact restore            # undo the last run
```

### What counts as "too frequent"

| Flag | Default | Meaning |
| --- | --- | --- |
| `--by` | `burst` | `burst`: a release within `--gap` of the previous one joins its run. `minor`: every `vX.Y.*` folds into the newest `vX.Y.*`. `count`: fixed chunks of `--max-group`. |
| `--gap` | `1d` | Burst window. Takes `90m`, `12h`, `3d`, `2w`. |
| `-m, --max-group` | `4` | Most releases folded into one, counting ones folded in earlier runs (`0` = no limit). |
| `--min-releases` | `3` | Repos with fewer releases are left alone. |
| `-k, --keep` | `0` | Never touch the newest N releases. |
| `--min-age` | `1h` | Skip releases younger than this, so CI can finish uploading. |
| `--prereleases` | off | Fold prereleases too (drafts are never touched). |
| `--delete-tags` | off | Also delete the tags of removed releases. |
| `--forks` | off | Include forks. Archived repos are always skipped. |

### What the merged notes look like

The newest release's own notes stay on top, untouched. Below them:

> ### 📦 Also includes `v1.3.1` → `v1.3.3`
> **✨ New** · **🐛 Fixed** · **🔧 Changed** — the first sentence of every bullet, de-duplicated
>
> ▸ **v1.3.3** · 2026-10-31 — full original notes, collapsed
> ▸ **v1.3.2** · 2026-10-31
> ▸ **v1.3.1** · 2026-10-31

Compacting again later never nests: earlier folded notes are carried forward as
their own entries.

## Failing checks: `gh compact checks`

```bash
gh compact checks            # every repo you own: diagnose, ask, fix, re-run, wait
gh compact checks -n         # only diagnose
gh compact checks pairdesk   # one repo
gh compact checks --pr       # send code fixes as pull requests
```

It reads the latest run of every workflow on each repo's default branch, pulls
the failed jobs' logs, and names the cause:

| Cause | What it does |
| --- | --- |
| **flaky** — runner shut down, network/DNS timeouts, 5xx, rate limits, cancelled or timed out | re-runs the failed jobs |
| **vulnerability** — govulncheck found a CVE that has a fixed version | standard library: bumps the `toolchain` line in `go.mod`; modules: `go get module@fixed` + `go mod tidy`. Commits (or opens a PR) and the check runs again |
| **gh extension** — a `gh-*` repo's latest release has no binaries, so `gh extension install` fails | re-runs that release's workflow |
| **tests · build · lint · dependencies · workflow config** (missing secret, permissions, bad action version, invalid YAML) | shows the lines that matter and what to do |

```text
◆ you/pairdesk · govulncheck  failed 1.2h ago  vulnerability
  ✗ 1 known vulnerability (GO-2026-6859)
    GO-2026-6859 in net/http@go1.27.3 → fixed in go1.27.4
  → will bump go.mod toolchain go1.27.3 → go1.27.4, commit to main, re-run
```

After you say yes it waits (up to `--timeout 20m`) and reports **✓ passed** or
**✗ failure** for each check. If a workflow doesn't run on push, it starts it by
hand when the workflow allows `workflow_dispatch`. Commits use your
`git config user.name/user.email`. `--rerun-unknown` also re-runs failures it
can't explain; `--since 90d` ignores older ones; `--no-wait` skips waiting.
Module fixes need `git` and `go` installed locally; everything else works through
the API.

## Safety

- **You see the plan first.** Nothing changes until you say yes (or pass `--yes`).
- **Notes before deletes.** The surviving release is updated first; if that fails,
  nothing in that group is deleted.
- **Backup every run** to `~/.local/state/gh-compact/backup-*.json` (every
  release's name, tag, notes and tag commit). `gh compact restore [file]`
  recreates deleted releases and puts the old notes back.
- **Tags stay** by default, so `git checkout v0.1.15` still works.
- What restore can't bring back: **assets** of deleted releases (the plan shows how
  many and how big), and original publish dates.
- Repos you can only read are planned but never touched.

## Run it on a schedule

```yaml
# .github/workflows/compact.yml
on:
  schedule: [{ cron: "0 3 * * 1" }]
  workflow_dispatch:
jobs:
  compact:
    runs-on: ubuntu-latest
    steps:
      - run: gh extension install Kumarsujay03/gh-compact
        env: { GH_TOKEN: "${{ secrets.COMPACT_TOKEN }}" }
      - run: gh compact --yes --gap 2d
        env: { GH_TOKEN: "${{ secrets.COMPACT_TOKEN }}" }
```

`COMPACT_TOKEN` is a personal access token with the `repo` scope (the default
`GITHUB_TOKEN` can only see its own repo).

## Build

```bash
go test ./... && go build -o gh-compact . && ./gh-compact --dry-run
```

MIT licensed.
