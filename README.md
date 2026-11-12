# gh compact

**Fold too-frequent GitHub releases into tidy ones — across all your repos, in one command.**

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
| `-m, --max-group` | `4` | Most releases folded into one (`0` = no limit). |
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
