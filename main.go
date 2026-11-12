package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/safeexec"
	flag "github.com/spf13/pflag"
)

var version = "dev"

type durationValue struct{ d *time.Duration }

// Set accepts Go durations plus days and weeks: 90m, 12h, 3d, 2w.
func (v durationValue) Set(s string) error {
	s = strings.TrimSpace(s)
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			f, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return fmt.Errorf("bad duration %q", s)
			}
			*v.d = time.Duration(f * float64(unit))
			return nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q (try 12h, 3d, 2w)", s)
	}
	*v.d = d
	return nil
}
func (v durationValue) String() string { return shortDuration(*v.d) }

func shortDuration(d time.Duration) string {
	switch {
	case d > 0 && d%(7*24*time.Hour) == 0:
		return fmt.Sprintf("%dw", d/(7*24*time.Hour))
	case d > 0 && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// printFlags lists flags without pflag's "[=true|false]" noise.
func printFlags(fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		if f.Hidden {
			return
		}
		name := "    --" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", --" + f.Name
		}
		if t := f.Value.Type(); t != "bool" {
			name += " " + map[string]string{"stringSlice": "string", "duration": "time"}[t]
			if strings.HasSuffix(name, " ") {
				name += t
			}
		}
		def := ""
		if f.Value.Type() != "bool" && f.DefValue != "" && f.DefValue != "0" && f.DefValue != "[]" {
			def = " (default " + f.DefValue + ")"
		}
		fmt.Fprintf(os.Stderr, "  %-24s %s%s\n", name, f.Usage, def)
	})
}
func (v durationValue) Type() string { return "duration" }

type cli struct {
	plan       PlanOptions
	dryRun     bool
	yes        bool
	preview    bool
	deleteTags bool
	forks      bool
	orgs       []string
	repos      []string
}

const overview = `gh compact — tidy GitHub releases and fix failing checks, across all your repos.

COMMANDS
  gh compact [repo ...]            Fold too-frequent releases into tidy ones.
                                   Shows the plan, asks, then compacts.
  gh compact checks [repo ...]     Find failing GitHub checks, explain why,
                                   fix what it can, re-run, wait for the result.
  gh compact restore [backup]      Undo the last compaction (or a given backup).
  gh compact help [command]        This help, or one command's help + flags.
  gh compact version               Print the version.

MOST USEFUL
  gh compact -n                    see which releases would be folded (no changes)
  gh compact                       compact every repo you own (asks first)
  gh compact pairdesk -p           one repo, preview the new release notes
  gh compact checks -n             see failing checks and why (no changes)
  gh compact checks                fix + re-run failing checks (asks first)
  gh compact checks pairdesk -y    fix one repo without asking
  gh compact restore               undo the last compaction

  Repos: "name" means one of yours, or give "owner/name".
  More: gh compact help compact · gh compact help checks
`

const usage = `gh compact — fold too-frequent GitHub releases into tidy ones.

Scans your repositories (private ones too), finds releases that came out
too close together, and folds each run of them into the newest one: its
notes gain a summary plus the full notes of every release it absorbed,
then the older releases are deleted. Tags stay unless --delete-tags.
A backup is written first; ` + "`gh compact restore`" + ` undoes a run.

USAGE
  gh compact [flags] [owner/repo | repo ...]

EXAMPLES
  gh compact                      every repo you own: plan, ask, compact
  gh compact -n                   only show the plan
  gh compact pairdesk -p          one repo, preview the merged notes
  gh compact you/a you/b          just these repos
  gh compact --by minor           fold all v1.4.x into the newest v1.4.x
  gh compact --gap 3d -m 0        everything within 3 days, no size limit
  gh compact -k 1                 never touch the newest release
  gh compact --org my-team -y     include an organisation, no questions

FLAGS
`

func parseArgs(args []string) (cli, error) {
	c := cli{plan: PlanOptions{Strategy: ByBurst, Gap: 24 * time.Hour, MaxGroup: 4, MinReleases: 3, MinAge: time.Hour}}
	fs := flag.NewFlagSet("gh compact", flag.ContinueOnError)
	fs.SortFlags = false
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		printFlags(fs)
	}
	fs.BoolVarP(&c.dryRun, "dry-run", "n", false, "show the plan and change nothing")
	fs.BoolVarP(&c.yes, "yes", "y", false, "compact without asking (for scripts and CI)")
	fs.BoolVarP(&c.preview, "preview", "p", false, "print the merged release notes of every group")
	fs.StringVar(&c.plan.Strategy, "by", c.plan.Strategy, "how to group: burst (close in time), minor (same vX.Y), count (fixed chunks)")
	fs.Var(durationValue{&c.plan.Gap}, "gap", "burst: releases this close to the previous one count as too frequent")
	fs.IntVarP(&c.plan.MaxGroup, "max-group", "m", c.plan.MaxGroup, "most releases folded into one (0 = no limit)")
	fs.IntVar(&c.plan.MinReleases, "min-releases", c.plan.MinReleases, "leave repos with fewer releases than this alone")
	fs.IntVarP(&c.plan.Keep, "keep", "k", 0, "never touch the newest N releases")
	fs.Var(durationValue{&c.plan.MinAge}, "min-age", "leave releases younger than this alone (CI may still be uploading)")
	fs.BoolVar(&c.plan.Prereleases, "prereleases", false, "fold prereleases too")
	fs.BoolVar(&c.deleteTags, "delete-tags", false, "also delete the git tags of removed releases")
	fs.StringSliceVar(&c.orgs, "org", nil, "also scan this organisation's repos (repeatable)")
	fs.BoolVar(&c.forks, "forks", false, "include forks")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	c.repos = fs.Args()
	if c.plan.Gap <= 0 && c.plan.Strategy == ByBurst {
		return c, errors.New("--gap must be positive")
	}
	if c.plan.Strategy == ByCount && c.plan.MaxGroup < 2 {
		return c, errors.New("--by count needs --max-group 2 or more")
	}
	return c, nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, sErr.Render("✗ ")+err.Error())
			os.Exit(1)
		}
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			switch {
			case len(args) > 1 && (args[1] == "checks" || args[1] == "check"):
				return runChecks([]string{"--help"})
			case len(args) > 1 && args[1] == "restore":
				return runRestore([]string{"--help"})
			case len(args) > 1 && args[1] == "compact":
				_, err := parseArgs([]string{"--help"})
				return err
			}
			fmt.Fprint(os.Stderr, overview)
			return nil
		case "version", "--version", "-v":
			if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
				version = bi.Main.Version
			}
			fmt.Println("gh-compact", version)
			return nil
		case "restore":
			return runRestore(args[1:])
		case "checks", "check":
			return runChecks(args[1:])
		}
	}
	c, err := parseArgs(args)
	if err != nil {
		return err
	}
	c.plan.Now = time.Now()
	banner(os.Stderr)

	client, login, err := connect()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, sDim.Render("  signed in as ")+sBold.Render(login))

	repos, err := pickRepos(client, login, c)
	if err != nil {
		return err
	}
	plans := scan(client, repos, c.plan)

	var todo []RepoPlan
	var removed, assets int
	for _, p := range plans {
		if p.Err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", sWarn.Render("!"), p.Repo.FullName, p.Err)
			continue
		}
		if len(p.Groups) == 0 {
			continue
		}
		renderPlan(os.Stdout, p)
		if c.preview {
			for _, g := range p.Groups {
				printPreview(g)
			}
		}
		if p.Repo.CanWrite() {
			todo = append(todo, p)
			removed += p.Removed()
			for _, g := range p.Groups {
				n, _ := g.DroppedAssets()
				assets += n
			}
		}
	}

	scanned := fmt.Sprintf("%d %s scanned", len(repos), plural(len(repos), "repo", "repos"))
	if len(todo) == 0 {
		fmt.Println()
		fmt.Println(sOK.Render("✨ All tidy.") + sDim.Render(" "+scanned+", nothing released too often."))
		return nil
	}
	summary := fmt.Sprintf("%s compact %d %s · remove %d %s", sBold.Render("Plan:"), len(todo),
		plural(len(todo), "repo", "repos"), removed, plural(removed, "release", "releases"))
	if assets > 0 {
		summary += sWarn.Render(fmt.Sprintf(" · %d old assets go with them", assets))
	}
	if !c.deleteTags {
		summary += sDim.Render(" · tags kept")
	}
	fmt.Println()
	fmt.Println(sBox.Render(summary + "\n" + sDim.Render(scanned)))

	if c.dryRun {
		fmt.Println(sDim.Render("Dry run: nothing changed. Run without --dry-run to compact."))
		return nil
	}
	if !c.yes {
		if !interactive() {
			return errors.New("not a terminal: pass --yes to compact, or --dry-run to only look")
		}
		switch ask(sBrand.Render("? ") + "Compact now? " + sDim.Render("[y]es / [n]o / [p]ick per repo ") + "") {
		case "y", "yes":
		case "p", "pick":
			var picked []RepoPlan
			for _, p := range todo {
				if a := ask(fmt.Sprintf("  %s (−%d)? [y/N] ", sRepo.Render(p.Repo.FullName), p.Removed())); a == "y" || a == "yes" {
					picked = append(picked, p)
				}
			}
			todo = picked
		default:
			fmt.Println(sDim.Render("Nothing changed."))
			return nil
		}
		if len(todo) == 0 {
			fmt.Println(sDim.Render("Nothing picked, nothing changed."))
			return nil
		}
	}
	return apply(client, todo, c.deleteTags)
}

func connect() (*Client, string, error) {
	opts := api.ClientOptions{}
	try := func() (*Client, string, error) {
		cl, err := NewClient(opts)
		if err != nil {
			return nil, "", err
		}
		login, err := cl.Login()
		return cl, login, err
	}
	cl, login, err := try()
	if err == nil {
		return cl, login, nil
	}
	gh, lookErr := safeexec.LookPath("gh")
	if lookErr != nil || !interactive() {
		return nil, "", fmt.Errorf("not signed in to GitHub (%v)\n  run `gh auth login`, or set GH_TOKEN to a token with the repo scope", err)
	}
	fmt.Fprintln(os.Stderr, sWarn.Render("  not signed in to GitHub yet. Starting `gh auth login`…"))
	cmd := exec.Command(gh, "auth", "login", "--scopes", "repo")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("gh auth login failed: %w", err)
	}
	return try()
}

func pickRepos(cl *Client, login string, c cli) ([]Repo, error) {
	sp := startSpinner("finding repositories…")
	defer sp.Stop()
	if len(c.repos) > 0 {
		var out []Repo
		for _, name := range c.repos {
			name = strings.TrimSuffix(strings.TrimPrefix(name, "https://github.com/"), "/")
			if !strings.Contains(name, "/") {
				name = login + "/" + name
			}
			r, err := cl.Repo(name)
			if err != nil {
				if isNotFound(err) {
					return nil, fmt.Errorf("%s: not found (or your token cannot see it)", name)
				}
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out = append(out, r)
		}
		return out, nil
	}
	all, err := cl.OwnedRepos()
	if err != nil {
		return nil, err
	}
	for _, org := range c.orgs {
		rs, err := cl.OrgRepos(org)
		if err != nil {
			return nil, fmt.Errorf("org %s: %w", org, err)
		}
		all = append(all, rs...)
	}
	seen := map[string]bool{}
	var out []Repo
	for _, r := range all {
		if seen[r.FullName] || (r.Fork && !c.forks) || r.Archived {
			continue
		}
		seen[r.FullName] = true
		out = append(out, r)
	}
	return out, nil
}

func scan(cl *Client, repos []Repo, o PlanOptions) []RepoPlan {
	plans := make([]RepoPlan, len(repos))
	sp := startSpinner(fmt.Sprintf("reading releases of %d repos…", len(repos)))
	defer sp.Stop()
	var wg sync.WaitGroup
	var mu sync.Mutex
	doneN := 0
	sem := make(chan struct{}, 8)
	for i, r := range repos {
		wg.Add(1)
		go func(i int, r Repo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p := RepoPlan{Repo: r}
			rels, err := cl.Releases(r.FullName)
			if err == nil {
				p.Releases = len(rels)
				p.Groups, err = Plan(rels, o)
			}
			p.Err = err
			plans[i] = p
			mu.Lock()
			doneN++
			sp.Set(fmt.Sprintf("reading releases… %d/%d", doneN, len(repos)))
			mu.Unlock()
		}(i, r)
	}
	wg.Wait()
	sort.SliceStable(plans, func(i, j int) bool { return plans[i].Removed() > plans[j].Removed() })
	return plans
}

func printPreview(g Group) {
	body := MergeNotes(g)
	head := sTag.Render(fmt.Sprintf("── new notes for %s ", g.Survivor.TagName))
	fmt.Println("\n" + head + sDim.Render(strings.Repeat("─", 40)))
	fmt.Println(body)
	fmt.Println(sDim.Render(strings.Repeat("─", 60)))
}

func apply(cl *Client, todo []RepoPlan, deleteTags bool) error {
	b := Backup{Version: 1, CreatedAt: time.Now()}
	for _, p := range todo {
		rb := RepoBackup{Repo: p.Repo.FullName}
		for _, g := range p.Groups {
			gb := GroupBackup{Survivor: g.Survivor, Deleted: append([]Release(nil), g.Members...)}
			if deleteTags {
				for i, m := range gb.Deleted {
					if sha, err := cl.TagSHA(p.Repo.FullName, m.TagName); err == nil {
						gb.Deleted[i].TagSHA = sha
					}
				}
			}
			rb.Groups = append(rb.Groups, gb)
		}
		b.Repos = append(b.Repos, rb)
	}
	path, err := writeBackup(b)
	if err != nil {
		return fmt.Errorf("could not write the backup, nothing changed: %w", err)
	}
	fmt.Println()
	fmt.Println(sDim.Render("  backup → ") + path)

	var removed, failed int
	for _, p := range todo {
		repo := p.Repo.FullName
		for _, g := range p.Groups {
			// Notes first: if anything later fails, nothing has been lost.
			if err := cl.UpdateBody(repo, g.Survivor.ID, MergeNotes(g)); err != nil {
				failed++
				fmt.Printf("  %s %s %s: could not update notes, group skipped: %v\n", sErr.Render("✗"), repo, g.Survivor.TagName, err)
				continue
			}
			gone := 0
			for _, m := range g.Members {
				if err := cl.DeleteRelease(repo, m.ID); err != nil && !isNotFound(err) {
					failed++
					fmt.Printf("  %s %s %s: %v\n", sErr.Render("✗"), repo, m.TagName, err)
					continue
				}
				gone++
				if deleteTags {
					if err := cl.DeleteTag(repo, m.TagName); err != nil && !isNotFound(err) {
						fmt.Printf("  %s %s tag %s kept: %v\n", sWarn.Render("!"), repo, m.TagName, err)
					}
				}
			}
			removed += gone
			fmt.Printf("  %s %s %s %s\n", sOK.Render("✓"), sRepo.Render(repo), sKeep.Render(g.Survivor.TagName),
				sDim.Render(fmt.Sprintf("now holds %d %s", gone+1, plural(gone+1, "release", "releases"))))
		}
	}
	fmt.Println()
	msg := sOK.Render(fmt.Sprintf("✨ Done. Removed %d %s.", removed, plural(removed, "release", "releases")))
	fmt.Println(msg + sDim.Render(" Undo with: gh compact restore "+path))
	if failed > 0 {
		return fmt.Errorf("%d step(s) failed, see above", failed)
	}
	return nil
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("gh compact restore", flag.ContinueOnError)
	yes := fs.BoolP("yes", "y", false, "restore without asking")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gh compact restore [backup.json] [--yes]\n\nRecreates the releases a run deleted and puts back the old notes.\nAssets cannot come back, and recreated releases get today's date.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := fs.Arg(0)
	var err error
	if path == "" {
		if path, err = latestBackup(); err != nil {
			return err
		}
	}
	b, err := readBackup(path)
	if err != nil {
		return err
	}
	banner(os.Stderr)
	n := 0
	for _, r := range b.Repos {
		for _, g := range r.Groups {
			n += len(g.Deleted)
		}
	}
	fmt.Printf("  %s from %s: %d %s across %d %s\n", sBold.Render("restore"), path, n,
		plural(n, "release", "releases"), len(b.Repos), plural(len(b.Repos), "repo", "repos"))
	if !*yes {
		if !interactive() {
			return errors.New("not a terminal: pass --yes")
		}
		if a := ask(sBrand.Render("? ") + "Restore? [y/N] "); a != "y" && a != "yes" {
			fmt.Println(sDim.Render("Nothing changed."))
			return nil
		}
	}
	cl, _, err := connect()
	if err != nil {
		return err
	}
	return restoreBackup(cl, b)
}

func restoreBackup(cl *Client, b Backup) error {
	failed := 0
	for _, r := range b.Repos {
		existing, err := cl.Releases(r.Repo)
		if err != nil {
			failed++
			fmt.Printf("  %s %s: %v\n", sErr.Render("✗"), r.Repo, err)
			continue
		}
		have := map[string]bool{}
		for _, e := range existing {
			have[e.TagName] = true
		}
		for _, g := range r.Groups {
			for i := len(g.Deleted) - 1; i >= 0; i-- { // oldest first
				d := g.Deleted[i]
				if have[d.TagName] {
					fmt.Printf("  %s %s %s already there\n", sDim.Render("·"), r.Repo, d.TagName)
					continue
				}
				if d.TagSHA != "" {
					if err := cl.CreateTag(r.Repo, d.TagName, d.TagSHA); err != nil {
						fmt.Printf("  %s %s tag %s: %v\n", sWarn.Render("!"), r.Repo, d.TagName, err)
					}
				}
				if _, err := cl.CreateRelease(r.Repo, d); err != nil {
					failed++
					fmt.Printf("  %s %s %s: %v\n", sErr.Render("✗"), r.Repo, d.TagName, err)
					continue
				}
				fmt.Printf("  %s %s %s\n", sOK.Render("✓"), sRepo.Render(r.Repo), sKeep.Render(d.TagName))
			}
			if err := cl.UpdateBody(r.Repo, g.Survivor.ID, g.Survivor.Body); err != nil && !isNotFound(err) {
				failed++
				fmt.Printf("  %s %s %s notes: %v\n", sErr.Render("✗"), r.Repo, g.Survivor.TagName, err)
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d step(s) failed, see above", failed)
	}
	fmt.Println(sOK.Render("✨ Restored."))
	return nil
}
