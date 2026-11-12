package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cli/safeexec"
	flag "github.com/spf13/pflag"
)

type checksCLI struct {
	dryRun, yes, pr, noWait, rerunUnknown, forks bool
	since, timeout                               time.Duration
	orgs, repos                                  []string
}

// Failing is one workflow whose latest run on the default branch failed.
type Failing struct {
	Repo   Repo
	Run    Run
	Jobs   []string // failed job names
	Cause  Cause
	Change *FileChange // prepared edit for FixToolchain
	Err    error
}

type FileChange struct {
	Path, Old, New, SHA, Desc string
}

const checksUsage = `gh compact checks — find failing GitHub checks, say why, fix what can be fixed.

Looks at the latest run of every workflow on each repo's default branch.
For each failure it reads the failed jobs' logs and names the cause:

  flaky     runner/network/GitHub hiccup      → re-runs the failed jobs
  vuln      govulncheck found a fixed CVE     → bumps go.mod, commits, re-runs
  gh-ext    release has no binaries           → re-runs the release workflow
  test · build · lint · deps · config         → shows the lines and what to do

Nothing changes until you say yes. Then it waits for the checks and tells
you whether they passed.

USAGE
  gh compact checks [flags] [owner/repo | repo ...]

EXAMPLES
  gh compact checks               all your repos
  gh compact checks -n            only diagnose
  gh compact checks pairdesk -y   fix pairdesk without asking
  gh compact checks --pr          send fixes as pull requests

FLAGS
`

func runChecks(args []string) error {
	c := checksCLI{since: 90 * 24 * time.Hour, timeout: 20 * time.Minute}
	fs := flag.NewFlagSet("gh compact checks", flag.ContinueOnError)
	fs.SortFlags = false
	fs.Usage = func() { fmt.Fprint(os.Stderr, checksUsage); printFlags(fs) }
	fs.BoolVarP(&c.dryRun, "dry-run", "n", false, "diagnose only, change nothing")
	fs.BoolVarP(&c.yes, "yes", "y", false, "apply fixes without asking")
	fs.BoolVar(&c.pr, "pr", false, "open a pull request for code fixes instead of committing to the default branch")
	fs.BoolVar(&c.rerunUnknown, "rerun-unknown", false, "also re-run failures whose cause is unclear")
	fs.BoolVar(&c.noWait, "no-wait", false, "don't wait for the re-run checks to finish")
	fs.Var(durationValue{&c.since}, "since", "ignore failures older than this")
	fs.Var(durationValue{&c.timeout}, "timeout", "how long to wait for checks")
	fs.StringSliceVar(&c.orgs, "org", nil, "also check this organisation's repos (repeatable)")
	fs.BoolVar(&c.forks, "forks", false, "include forks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c.repos = fs.Args()

	bannerFor(os.Stderr, "find failing checks, fix and re-run them")
	client, login, err := connect()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, sDim.Render("  signed in as ")+sBold.Render(login))
	repos, err := pickRepos(client, login, cli{repos: c.repos, orgs: c.orgs, forks: c.forks})
	if err != nil {
		return err
	}
	fails := scanChecks(client, repos, c)

	var todo []*Failing
	manual := 0
	for _, f := range fails {
		renderFailing(f, c)
		if f.Err != nil {
			continue
		}
		if f.Repo.CanWrite() && f.fixable(c) {
			todo = append(todo, f)
		} else {
			manual++
		}
	}
	scanned := fmt.Sprintf("%d %s checked", len(repos), plural(len(repos), "repo", "repos"))
	if len(fails) == 0 {
		fmt.Println()
		fmt.Println(sOK.Render("✨ All green.") + sDim.Render(" "+scanned+", no failing checks."))
		return nil
	}
	fmt.Println()
	fmt.Println(sBox.Render(fmt.Sprintf("%s %d failing · %s · %s\n%s", sBold.Render("Checks:"), len(fails),
		sOK.Render(fmt.Sprintf("%d gh compact can fix", len(todo))),
		sWarn.Render(fmt.Sprintf("%d need you", manual)), sDim.Render(scanned))))
	if len(todo) == 0 || c.dryRun {
		if c.dryRun && len(todo) > 0 {
			fmt.Println(sDim.Render("Dry run: nothing changed. Run without -n to fix."))
		}
		return nil
	}
	if !c.yes {
		if !interactive() {
			return errors.New("not a terminal: pass --yes to fix, or -n to only diagnose")
		}
		switch ask(sBrand.Render("? ") + fmt.Sprintf("Fix %d now? ", len(todo)) + sDim.Render("[y]es / [n]o / [p]ick ")) {
		case "y", "yes":
		case "p", "pick":
			var picked []*Failing
			for _, f := range todo {
				if a := ask(fmt.Sprintf("  %s · %s: %s? [y/N] ", sRepo.Render(f.Repo.FullName), f.Run.Name, f.action(c))); a == "y" || a == "yes" {
					picked = append(picked, f)
				}
			}
			todo = picked
		default:
			fmt.Println(sDim.Render("Nothing changed."))
			return nil
		}
	}
	return fixChecks(client, todo, c)
}

func (f *Failing) fixable(c checksCLI) bool {
	switch f.Cause.Fix.Kind {
	case FixRerun:
		return f.Run.ID != 0
	case FixToolchain:
		return f.Change != nil
	case FixGoGet:
		return haveTools("git", "go", "gh") == nil
	}
	return c.rerunUnknown && f.Cause.Kind == KindUnknown
}

func (f *Failing) action(c checksCLI) string {
	where := "commit to " + f.Repo.Default
	if c.pr {
		where = "open a PR"
	}
	switch f.Cause.Fix.Kind {
	case FixToolchain:
		if f.Change != nil {
			return f.Change.Desc + ", " + where + ", re-run"
		}
	case FixGoGet:
		var mods []string
		for m, v := range f.Cause.Fix.Modules {
			mods = append(mods, m+"@"+v)
		}
		sort.Strings(mods)
		s := "run go get " + strings.Join(mods, " ")
		if f.Cause.Fix.Toolchain != "" {
			s += " + toolchain " + f.Cause.Fix.Toolchain
		}
		return s + ", " + where + ", re-run"
	}
	return "re-run the failed jobs"
}

func scanChecks(cl *Client, repos []Repo, c checksCLI) []*Failing {
	sp := startSpinner(fmt.Sprintf("reading checks of %d repos…", len(repos)))
	defer sp.Stop()
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		all   []*Failing
		doneN int
	)
	sem := make(chan struct{}, 6)
	for _, r := range repos {
		wg.Add(1)
		go func(r Repo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fs := checkRepo(cl, r, c)
			mu.Lock()
			all = append(all, fs...)
			doneN++
			sp.Set(fmt.Sprintf("reading checks… %d/%d", doneN, len(repos)))
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Repo.FullName != all[j].Repo.FullName {
			return all[i].Repo.FullName < all[j].Repo.FullName
		}
		return all[i].Run.Name < all[j].Run.Name
	})
	return all
}

func checkRepo(cl *Client, r Repo, c checksCLI) []*Failing {
	var out []*Failing
	if r.Default != "" {
		runs, err := cl.Runs(r.FullName, "branch="+escapeRef(r.Default)+"&per_page=60&exclude_pull_requests=true")
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return []*Failing{{Repo: r, Err: err}}
		}
		seen := map[int64]bool{}
		for _, run := range runs { // newest first
			if seen[run.WorkflowID] {
				continue
			}
			seen[run.WorkflowID] = true
			if run.Status != "completed" || !run.Failed() || time.Since(run.CreatedAt) > c.since {
				continue
			}
			out = append(out, diagnoseRun(cl, r, run))
		}
	}
	if strings.HasPrefix(strings.SplitN(r.FullName, "/", 2)[1], "gh-") {
		if f := checkGhExtension(cl, r); f != nil {
			out = append(out, f)
		}
	}
	return out
}

func diagnoseRun(cl *Client, r Repo, run Run) *Failing {
	f := &Failing{Repo: r, Run: run}
	jobs, err := cl.Jobs(r.FullName, run.ID)
	if err != nil {
		f.Err = err
		return f
	}
	var causes []Cause
	for _, j := range jobs {
		if j.Conclusion != "failure" && j.Conclusion != "timed_out" && j.Conclusion != "cancelled" {
			continue
		}
		f.Jobs = append(f.Jobs, j.Name)
		if len(causes) >= 3 {
			continue
		}
		raw, err := cl.JobLog(r.FullName, j.ID)
		if err != nil {
			causes = append(causes, Cause{Kind: KindUnknown, Title: "could not read the log: " + err.Error()})
			continue
		}
		concl := j.Conclusion
		if concl == "failure" {
			concl = run.Conclusion
		}
		causes = append(causes, Diagnose(concl, j.Name, cleanLog(raw)))
	}
	if len(causes) == 0 {
		causes = append(causes, Diagnose(run.Conclusion, run.Name, nil))
	}
	f.Cause = bestCause(causes)
	if f.Cause.Fix.Kind == FixToolchain {
		prepareToolchain(cl, f)
	}
	return f
}

// bestCause prefers a cause gh compact can fix, then a specific one over "unknown".
func bestCause(cs []Cause) Cause {
	rank := func(c Cause) int {
		switch {
		case c.Kind == KindVuln && c.Fix.Auto():
			return 0
		case c.Kind != KindUnknown && c.Kind != KindFlaky:
			return 1
		case c.Kind == KindFlaky:
			return 2
		}
		return 3
	}
	sort.SliceStable(cs, func(i, j int) bool { return rank(cs[i]) < rank(cs[j]) })
	return cs[0]
}

func prepareToolchain(cl *Client, f *Failing) {
	text, sha, err := cl.FileText(f.Repo.FullName, "go.mod", f.Repo.Default)
	if err != nil || text == "" {
		f.Cause.Hint += " (go.mod not found at the repo root, so gh compact can't bump it.)"
		f.Cause.Fix.Kind = FixNone
		return
	}
	updated, old, changed := bumpToolchain(text, f.Cause.Fix.Toolchain)
	if !changed {
		// go.mod is already new enough: the failed run is older than the fix.
		f.Cause.Hint = "go.mod already asks for " + old + "; a re-run should pass."
		f.Cause.Fix = Fix{Kind: FixRerun}
		return
	}
	f.Change = &FileChange{Path: "go.mod", Old: text, New: updated, SHA: sha,
		Desc: fmt.Sprintf("bump go.mod toolchain %s → %s", old, f.Cause.Fix.Toolchain)}
}

// checkGhExtension: a precompiled gh extension whose latest release has no
// binaries makes `gh extension install` fail.
func checkGhExtension(cl *Client, r Repo) *Failing {
	rel, err := cl.LatestRelease(r.FullName)
	if err != nil || len(rel.Assets) > 0 {
		return nil
	}
	name := strings.SplitN(r.FullName, "/", 2)[1]
	if script, _, _ := cl.FileText(r.FullName, name, r.Default); script != "" {
		return nil // a script extension needs no binaries
	}
	f := &Failing{Repo: r, Run: Run{Name: "gh extension install"}, Cause: Cause{
		Kind:  KindGhExt,
		Title: fmt.Sprintf("`gh extension install %s` fails: release %s has no binaries", r.FullName, rel.TagName),
		Hint:  "Add a release workflow (cli/gh-extension-precompile) and push the tag again.",
	}}
	runs, err := cl.Runs(r.FullName, "branch="+escapeRef(rel.TagName)+"&per_page=5")
	if err == nil && len(runs) > 0 {
		f.Run = runs[0]
		f.Run.Name = "gh extension install (" + runs[0].Name + " " + rel.TagName + ")"
		f.Cause.Hint = "The release workflow for " + rel.TagName + " did not upload binaries; re-running it should."
		f.Cause.Fix = Fix{Kind: FixRerun}
	}
	return f
}

func renderFailing(f *Failing, c checksCLI) {
	if f.Err != nil {
		fmt.Fprintf(os.Stderr, "%s %s: %v\n", sWarn.Render("!"), f.Repo.FullName, f.Err)
		return
	}
	age := ""
	if !f.Run.CreatedAt.IsZero() {
		age = "failed " + human(time.Since(f.Run.CreatedAt)) + " ago"
	}
	fmt.Printf("\n%s %s %s %s  %s\n", sBrand.Render("◆"), sRepo.Render(f.Repo.FullName), sDim.Render("·"),
		sBold.Render(f.Run.Name), sDim.Render(strings.TrimSpace(age+"  "+kindBadge(f.Cause.Kind))))
	fmt.Printf("  %s %s\n", sErr.Render("✗"), f.Cause.Title)
	for i, l := range f.Cause.Lines {
		if i == 6 {
			fmt.Printf("    %s\n", sDim.Render(fmt.Sprintf("… %d more", len(f.Cause.Lines)-6)))
			break
		}
		if l != f.Cause.Title {
			fmt.Printf("    %s\n", sDim.Render(l))
		}
	}
	if f.Repo.CanWrite() && f.fixable(c) {
		fmt.Printf("  %s %s\n", sOK.Render("→ will"), f.action(c))
	} else {
		fmt.Printf("  %s %s\n", sWarn.Render("→ you:"), f.Cause.Hint)
	}
	if f.Run.HTMLURL != "" {
		fmt.Printf("  %s\n", sDim.Render(f.Run.HTMLURL))
	}
}

func kindBadge(k string) string {
	return map[string]string{KindFlaky: "flaky", KindVuln: "vulnerability", KindTest: "tests", KindBuild: "build",
		KindLint: "lint", KindDeps: "dependencies", KindConfig: "workflow config", KindGhExt: "gh extension", KindUnknown: "unclear"}[k]
}

// watch is one check to wait for after a fix.
type watch struct {
	f     *Failing
	runID int64  // re-run: same run id
	sha   string // commit: the new commit
	since time.Time
}

func fixChecks(cl *Client, todo []*Failing, c checksCLI) error {
	fmt.Println()
	var watches []watch
	failed := 0
	for _, f := range todo {
		label := sRepo.Render(f.Repo.FullName) + " " + f.Run.Name
		switch f.Cause.Fix.Kind {
		case FixToolchain, FixGoGet:
			sha, where, err := commitFix(cl, f, c)
			if err != nil {
				failed++
				fmt.Printf("  %s %s: %v\n", sErr.Render("✗"), label, err)
				continue
			}
			fmt.Printf("  %s %s %s\n", sOK.Render("✓"), label, sDim.Render("fix committed → "+where))
			watches = append(watches, watch{f: f, sha: sha, since: time.Now()})
		default:
			if err := cl.Rerun(f.Repo.FullName, f.Run.ID); err != nil {
				failed++
				fmt.Printf("  %s %s: re-run refused: %v\n", sErr.Render("✗"), label, err)
				continue
			}
			fmt.Printf("  %s %s %s\n", sOK.Render("✓"), label, sDim.Render("re-run started"))
			watches = append(watches, watch{f: f, runID: f.Run.ID, since: time.Now()})
		}
	}
	if len(watches) > 0 && !c.noWait {
		failed += waitChecks(cl, watches, c.timeout)
	} else if len(watches) > 0 {
		fmt.Println(sDim.Render("\nNot waiting. See the results later with: gh compact checks -n"))
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) still need attention", failed)
	}
	return nil
}

func commitAuthor() map[string]string {
	git, err := safeexec.LookPath("git")
	if err != nil {
		return nil
	}
	get := func(k string) string {
		out, _ := exec.Command(git, "config", "--get", k).Output()
		return strings.TrimSpace(string(out))
	}
	name, email := get("user.name"), get("user.email")
	if name == "" || email == "" {
		return nil // GitHub uses the token's account
	}
	return map[string]string{"name": name, "email": email}
}

func fixMessage(f *Failing) string {
	what := "Update vulnerable dependencies"
	if f.Cause.Fix.Kind == FixToolchain {
		what = "Bump Go toolchain to " + f.Cause.Fix.Toolchain
	}
	return fmt.Sprintf("%s\n\nFixes %s, reported by the %q check.", what, strings.Join(f.Cause.Fix.IDs, ", "), f.Run.Name)
}

// commitFix applies a code fix and returns the commit to watch and where it went.
func commitFix(cl *Client, f *Failing, c checksCLI) (sha, where string, err error) {
	repo, base := f.Repo.FullName, f.Repo.Default
	branch := base
	if c.pr {
		branch = fmt.Sprintf("gh-compact/fix-%s-%d", strings.ToLower(f.Cause.Kind), time.Now().Unix())
	}
	msg := fixMessage(f)
	if f.Cause.Fix.Kind == FixToolchain {
		if c.pr {
			head, err := cl.BranchSHA(repo, base)
			if err != nil {
				return "", "", err
			}
			if err := cl.CreateBranch(repo, branch, head); err != nil {
				return "", "", err
			}
		}
		sha, err = cl.PutFile(repo, f.Change.Path, branch, f.Change.SHA, msg, f.Change.New, commitAuthor())
	} else {
		sha, err = goGetFix(f, branch)
	}
	if err != nil {
		return "", "", err
	}
	where = base
	if c.pr {
		title, _, _ := strings.Cut(msg, "\n")
		url, err := cl.CreatePR(repo, branch, base, title, msg+"\n\nOpened by [gh compact checks](https://github.com/Kumarsujay03/gh-compact).")
		if err != nil {
			return "", "", fmt.Errorf("pushed %s but could not open the PR: %w", branch, err)
		}
		where = url
	}
	return sha, where, nil
}

func haveTools(names ...string) error {
	for _, n := range names {
		if _, err := safeexec.LookPath(n); err != nil {
			return fmt.Errorf("%s is not installed", n)
		}
	}
	return nil
}

// goGetFix clones the repo, updates modules with the Go tool and pushes.
func goGetFix(f *Failing, branch string) (string, error) {
	if err := haveTools("git", "go", "gh"); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "gh-compact-fix-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "repo")
	run := func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Dir = src
		if name == "gh" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := run("gh", "repo", "clone", f.Repo.FullName, src, "--", "--depth", "1", "--branch", f.Repo.Default); err != nil {
		return "", err
	}
	if tc := f.Cause.Fix.Toolchain; tc != "" {
		data, err := os.ReadFile(filepath.Join(src, "go.mod"))
		if err != nil {
			return "", err
		}
		if updated, _, changed := bumpToolchain(string(data), tc); changed {
			if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte(updated), 0o644); err != nil {
				return "", err
			}
		}
	}
	for m, v := range f.Cause.Fix.Modules {
		if err := run("go", "get", m+"@"+v); err != nil {
			return "", err
		}
	}
	if err := run("go", "mod", "tidy"); err != nil {
		return "", err
	}
	if err := run("git", "commit", "-am", fixMessage(f)); err != nil {
		return "", err
	}
	if err := run("git", "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return "", err
	}
	out, err := exec.Command("git", "-C", src, "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(out)), err
}

var pollEvery = 10 * time.Second

// waitChecks polls until every watched check finishes; returns how many failed.
func waitChecks(cl *Client, ws []watch, timeout time.Duration) int {
	fmt.Println()
	sp := startSpinner("waiting for checks…")
	deadline := time.Now().Add(timeout)
	type result struct {
		w   watch
		run Run
		msg string
	}
	results := map[int]result{}
	dispatched := map[int]bool{}
	for len(results) < len(ws) && time.Now().Before(deadline) {
		for i, w := range ws {
			if _, done := results[i]; done {
				continue
			}
			repo := w.f.Repo.FullName
			var run Run
			var err error
			if w.runID != 0 {
				run, err = cl.Run(repo, w.runID)
			} else {
				var runs []Run
				runs, err = cl.Runs(repo, "head_sha="+w.sha+"&per_page=30")
				for _, r := range runs {
					if r.WorkflowID == w.f.Run.WorkflowID {
						run = r
						break
					}
				}
				if run.ID == 0 && time.Since(w.since) > 3*pollEvery && !dispatched[i] {
					// The workflow doesn't run on push: start it by hand if it allows that.
					dispatched[i] = true
					if derr := cl.Dispatch(repo, w.f.Run.WorkflowID, w.f.Repo.Default); derr != nil {
						results[i] = result{w: w, msg: "committed, but this workflow doesn't run on push and can't be started by hand; it will run on its next trigger"}
					}
				}
			}
			if err == nil && run.ID != 0 && run.Status == "completed" {
				results[i] = result{w: w, run: run}
			}
		}
		sp.Set(fmt.Sprintf("waiting for checks… %d/%d done", len(results), len(ws)))
		if len(results) < len(ws) {
			time.Sleep(pollEvery)
		}
	}
	sp.Stop()
	bad := 0
	for i, w := range ws {
		label := sRepo.Render(w.f.Repo.FullName) + " " + w.f.Run.Name
		r, ok := results[i]
		switch {
		case !ok:
			bad++
			fmt.Printf("  %s %s %s\n", sWarn.Render("…"), label, sDim.Render("still running after "+human(timeout)))
		case r.msg != "":
			fmt.Printf("  %s %s %s\n", sWarn.Render("!"), label, sDim.Render(r.msg))
		case r.run.Conclusion == "success":
			fmt.Printf("  %s %s %s\n", sOK.Render("✓ passed"), label, sDim.Render(r.run.HTMLURL))
		default:
			bad++
			fmt.Printf("  %s %s %s %s\n", sErr.Render("✗ "+r.run.Conclusion), label, sDim.Render(r.run.HTMLURL),
				sDim.Render("· run gh compact checks again to see why"))
		}
	}
	return bad
}
