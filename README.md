# gh compact

**Tidy GitHub releases and fix failing checks, across all your repos, from one command.**

*Last updated: 12 November 2026 · v0.2.1*

| You want to… | Run |
| --- | --- |
| See which releases came out too often (changes nothing) | `gh compact -n` |
| Fold them into tidy releases | `gh compact` |
| See which GitHub checks are failing, and why (changes nothing) | `gh compact checks -n` |
| Fix and re-run the failing checks | `gh compact checks` |
| Undo the last compaction | `gh compact restore` |
| Get help in the terminal | `gh compact help` |

Every command shows you what it will do and asks before changing anything.
Private repos work too.

- [Install](#install)
- [Commands at a glance](#commands-at-a-glance)
- [`gh compact`: fold frequent releases](#gh-compact-fold-frequent-releases)
- [`gh compact checks`: fix failing checks](#gh-compact-checks-fix-failing-checks)
- [`gh compact restore`: undo](#gh-compact-restore-undo)
- [Recipes](#recipes)
- [Safety](#safety)
- [Run it on a schedule](#run-it-on-a-schedule)
- [Troubleshooting](#troubleshooting)

## Install

You need the [GitHub CLI](https://cli.github.com) (`gh`). Then:

```bash
gh extension install Kumarsujay03/gh-compact
```

`gh compact` uses your `gh` login. If you aren't signed in yet, it starts
`gh auth login` for you.

```bash
gh extension upgrade compact      # get the latest version
gh compact version                # see which version you have
gh extension remove compact       # uninstall
```

Without `gh`: run `go install github.com/Kumarsujay03/gh-compact@latest` and set
`GH_TOKEN` to a token with the `repo` scope.

## Commands at a glance

| Command | What it does | When to use it |
| --- | --- | --- |
| `gh compact [repo…]` | Finds runs of releases that came out too close together and folds each run into its newest release (merged notes, older releases deleted). | You shipped many small releases and the Releases page is noisy. |
| `gh compact checks [repo…]` | Reads the failed GitHub Actions runs, names the cause, fixes what it can (re-run, Go toolchain or module bump), re-runs and waits for ✓/✗. | A check went red and you want to know why, or get it green fast. |
| `gh compact restore [backup]` | Recreates the releases a compaction deleted and puts the old notes back. | You compacted something you didn't mean to. |
| `gh compact help [command]` | Overview, or one command's help with all its flags. | You forgot a flag. |
| `gh compact version` | Prints the installed version. | Checking that an upgrade worked. |

**Picking repos (all commands):** with no repo given, every repo you own is
checked (archived repos and forks are skipped). Give `name` for one of your own
repos, or `owner/name` for any repo you can access. `--org NAME` adds an
organisation's repos.

**Shared flags:** `-n` / `--dry-run` only shows what would happen (`compact`
and `checks`). `-y` / `--yes` skips the question (all three). With neither, you get a
prompt: **y**es, **n**o, or **p**ick repo by repo.

## `gh compact`: fold frequent releases

Shipped `v0.1.10` through `v0.1.17` in one afternoon? `gh compact` folds each run
of close releases into the newest one:

- the newest release **survives**, keeping its tag, its files and its "Latest" badge;
- its notes stay on top, followed by a **summary** of everything it absorbed
  (✨ New / 🐛 Fixed / 🔧 Changed, repeats removed) and each older release's
  **full notes** in a collapsed section;
- the older releases are **deleted**. Their git tags are kept.

```text
◆ you/pairdesk  8 releases → 2
  ├─ v0.1.17 ⟵ v0.1.16 v0.1.15 v0.1.14  spans 57m · drops 12 old assets (32.7 MiB)
  └─ v0.1.13 ⟵ v0.1.12 v0.1.11 v0.1.10  spans 51m · drops 12 old assets (32.7 MiB)

╭──────────────────────────────────────────────────────────────╮
│  Plan: compact 1 repo · remove 6 releases · tags kept        │
╰──────────────────────────────────────────────────────────────╯
? Compact now? [y]es / [n]o / [p]ick per repo
```

### Examples

```bash
gh compact -n                     # plan for every repo you own, change nothing
gh compact                        # plan, ask, compact
gh compact pairdesk -p            # one repo; -p prints the merged notes before asking
gh compact you/a you/b            # just these repos
gh compact --by minor             # fold every v1.4.x into the newest v1.4.x
gh compact --gap 3d -m 0          # anything within 3 days of the previous, no size limit
gh compact -k 1                   # never touch the newest release
gh compact --by count -m 3        # fixed groups of 3, however far apart
gh compact --delete-tags          # also delete the tags of removed releases
gh compact --org my-team -y       # include an organisation, no questions
```

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-n, --dry-run` | off | Show the plan, change nothing. |
| `-y, --yes` | off | Compact without asking. |
| `-p, --preview` | off | Print the new notes of every surviving release. |
| `--by` | `burst` | How to group. `burst`: a release within `--gap` of the previous one joins its run. `minor`: same `vX.Y`. `count`: fixed chunks of `--max-group`. |
| `--gap` | `1d` | Burst window. Takes `90m`, `12h`, `3d`, `2w`. |
| `-m, --max-group` | `4` | Most releases folded into one, counting ones folded in earlier runs. `0` = no limit. |
| `--min-releases` | `3` | Repos with fewer releases are left alone. |
| `-k, --keep` | `0` | Never touch the newest N releases. |
| `--min-age` | `1h` | Skip releases younger than this, so CI can finish uploading. |
| `--prereleases` | off | Fold prereleases too. Drafts are never touched. |
| `--delete-tags` | off | Also delete the git tags of removed releases. |
| `--org` | — | Also scan this organisation (repeatable). |
| `--forks` | off | Include forks. |

Running it again later is safe. A release that already holds 4 counts as 4, so
`--max-group` still holds, and earlier folded notes are carried forward, not
nested.

## `gh compact checks`: fix failing checks

Looks at the **latest run of every workflow** on each repo's default branch.
For each failure it reads the failed jobs' logs and names the cause:

| Cause | How it's spotted | What gh compact does |
| --- | --- | --- |
| **flaky** | runner shut down, network/DNS timeout, GitHub 5xx, rate limit, run cancelled or timed out | **re-runs** the failed jobs |
| **vulnerability** | govulncheck reports a vulnerability with a fixed version | Go standard library: **bumps `toolchain` in go.mod**. Modules: **`go get module@fixed` + `go mod tidy`**. Then commits to the default branch (or opens a PR with `--pr`), and the check runs again |
| **gh extension** | a `gh-*` repo's latest release has no binaries, so `gh extension install` would fail | **re-runs** that release's workflow |
| **tests** | `--- FAIL: TestX` | shows the failing tests and the command to run them locally |
| **build** | `file.go:12:3: …` compile errors | shows file:line and the message |
| **lint** | golangci-lint, gofmt, eslint, ruff… | shows the lines; tells you to run the linter |
| **dependencies** | Dependabot errors, unresolvable versions, missing go.sum | shows the message and the file to fix |
| **workflow config** | missing secret, missing permission, bad action version, invalid YAML | says exactly which setting to change |
| **unclear** | none of the above | shows the error lines; `--rerun-unknown` re-runs it anyway |

After you say yes it **waits** for every check it touched (up to `--timeout`)
and reports **✓ passed** or **✗ failure** with the run link.

```text
◆ you/pairdesk · govulncheck  failed 1.4h ago  vulnerability
  ✗ 1 known vulnerability (GO-2026-6859)
    GO-2026-6859 in net/http@go1.27.3 → fixed in go1.27.4
  → will bump go.mod toolchain go1.27.3 → go1.27.4, commit to main, re-run

  ✓ you/pairdesk govulncheck fix committed → main
  ✓ passed you/pairdesk govulncheck https://github.com/you/pairdesk/actions/runs/…
```

### Examples

```bash
gh compact checks -n              # what's failing everywhere, and why; change nothing
gh compact checks                 # diagnose, ask, fix, re-run, wait
gh compact checks pairdesk        # one repo
gh compact checks pairdesk -y     # one repo, fix without asking
gh compact checks --pr            # code fixes as pull requests instead of commits
gh compact checks --rerun-unknown # also re-run failures it can't explain
gh compact checks --no-wait       # start the fixes, don't wait for results
gh compact checks --since 7d      # ignore failures older than a week
```

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-n, --dry-run` | off | Diagnose only. |
| `-y, --yes` | off | Apply fixes without asking. |
| `--pr` | off | Open a pull request for code fixes instead of committing to the default branch. |
| `--rerun-unknown` | off | Also re-run failures with an unclear cause. |
| `--no-wait` | off | Don't wait for the re-run checks. |
| `--since` | `90d` | Ignore failures older than this. |
| `--timeout` | `20m` | How long to wait for checks. |
| `--org` | — | Also check this organisation (repeatable). |
| `--forks` | off | Include forks. |

Notes:
- Commits use your `git config user.name` / `user.email`.
- If a workflow doesn't run on push, it is started by hand when it allows
  `workflow_dispatch`.
- Module fixes need `git` and `go` installed. Re-runs and toolchain bumps work
  through the API alone.
- If you have unpushed local commits in a repo it fixed, run `git pull --rebase` there.

## `gh compact restore`: undo

Every compaction first writes a backup to
`~/.local/state/gh-compact/backup-YYYYMMDD-HHMMSS.json`.

```bash
gh compact restore                # undo the most recent compaction (asks first)
gh compact restore ~/.local/state/gh-compact/backup-20261112-140159.json
gh compact restore -y             # no question
ls ~/.local/state/gh-compact/     # list your backups
```

It recreates the deleted releases with their names, notes and tags, and puts the
surviving releases' old notes back. It **can't** bring back files attached to
deleted releases, and recreated releases get today's date.

## Recipes

| Situation | Command |
| --- | --- |
| Weekly clean-up of everything | `gh compact -n`, read the plan, then `gh compact` |
| A CI that tags every push | `gh compact --gap 1h -m 0` (one release per burst) |
| Keep only the latest of each minor line | `gh compact --by minor -m 0` |
| Don't touch the current release | `gh compact -k 1` |
| A check is red and you don't know why | `gh compact checks <repo> -n` |
| govulncheck is failing | `gh compact checks <repo>` (bumps Go, re-runs) |
| Prefer reviewing fixes first | `gh compact checks --pr` |
| A runner flaked | `gh compact checks <repo> -y` |
| Undo | `gh compact restore` |

## Safety

- **You see the plan first.** Nothing changes until you say yes (or pass `--yes`).
- **Notes before deletes.** The surviving release is updated first. If that fails,
  nothing in that group is deleted.
- **Backup every compaction.** `gh compact restore` undoes it.
- **Tags stay** by default, so `git checkout v0.1.15` still works.
- **Lost for good when compacting:** files attached to deleted releases (the plan
  shows how many and how big).
- **Checks fixes are small and named:** one go.mod line or a `go get`, shown
  before you confirm. Use `--pr` to review them first.
- Repos you can only read are diagnosed and planned but never changed.

## Run it on a schedule

```yaml
# .github/workflows/tidy.yml
on:
  schedule: [{ cron: "0 3 * * 1" }]
  workflow_dispatch:
jobs:
  tidy:
    runs-on: ubuntu-latest
    env: { GH_TOKEN: "${{ secrets.COMPACT_TOKEN }}" }
    steps:
      - run: gh extension install Kumarsujay03/gh-compact
      - run: gh compact --yes --gap 2d
      - run: gh compact checks --yes --pr --no-wait
```

`COMPACT_TOKEN` is a personal access token with the `repo` and `workflow` scopes.
The default `GITHUB_TOKEN` can only see its own repo. Backups written in CI stay
on the runner, so prefer running `gh compact` locally if you may want to undo.

## Troubleshooting

| Message | Fix |
| --- | --- |
| `not signed in to GitHub` | `gh auth login` (or set `GH_TOKEN`) |
| `not a terminal: pass --yes` | You're in a script: add `-y`, or `-n` to only look |
| `read-only · plan only` | You can't push to that repo; nothing will be changed there |
| `re-run refused` | GitHub won't re-run runs older than 30 days; push a commit instead |
| `this workflow doesn't run on push` | It will run on its next schedule; or add `workflow_dispatch:` to it |
| Want more detail on a check | Open the run link printed under it |

## Build from source

```bash
git clone https://github.com/Kumarsujay03/gh-compact && cd gh-compact
go test ./... && go build -o gh-compact . && ./gh-compact help
```

MIT licensed.
