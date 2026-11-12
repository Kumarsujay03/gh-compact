package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Cause kinds, from "just press re-run" to "a person must look".
const (
	KindFlaky   = "flaky"   // runner, network or GitHub hiccup: re-run fixes it
	KindVuln    = "vuln"    // govulncheck found a vulnerability with a fixed version
	KindTest    = "test"    // a test failed
	KindBuild   = "build"   // the code does not compile
	KindLint    = "lint"    // formatting or linter
	KindDeps    = "deps"    // dependency install / Dependabot
	KindConfig  = "config"  // workflow, secrets, permissions, actions
	KindGhExt   = "gh-ext"  // `gh extension install` would fail
	KindUnknown = "unknown" // could not tell
)

// Fix kinds gh compact can apply on its own.
const (
	FixNone      = ""
	FixRerun     = "rerun"     // re-run the failed jobs
	FixToolchain = "toolchain" // bump the go.mod toolchain line
	FixGoGet     = "goget"     // go get module@version
)

type Cause struct {
	Kind  string
	Title string   // one line: what went wrong
	Hint  string   // what to do about it
	Lines []string // the log lines that show it
	Fix   Fix
}

type Fix struct {
	Kind      string
	Toolchain string            // FixToolchain: e.g. go1.27.4
	Modules   map[string]string // FixGoGet: module -> fixed version
	IDs       []string          // vulnerability IDs, for the commit message
}

func (f Fix) Auto() bool { return f.Kind != FixNone }

var (
	tsRe   = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[\d:.]+Z ?`)
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
)

// cleanLog strips timestamps and colours from a raw Actions job log.
func cleanLog(raw string) []string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = ansiRe.ReplaceAllString(tsRe.ReplaceAllString(l, ""), "")
		out = append(out, strings.TrimRight(l, " \t"))
	}
	return out
}

var (
	errLineRe = regexp.MustCompile(`(?i)(^##\[error\]|^\s*(error|fatal|panic)(\[|:| )|^--- FAIL|^FAIL\s|npm ERR!|^\s*\S+\.(go|py|rs|ts|js|tsx|c|cc|cpp|java):\d+(:\d+)?: |Traceback \(most recent|AssertionError|Error: )`)
	exitRe    = regexp.MustCompile(`^##\[error\]Process completed with exit code \d+\.?$`)

	flakyRe    = regexp.MustCompile(`(?i)(runner has received a shutdown signal|lost communication with the server|the operation was canceled|exceeded the maximum execution time|connection reset by peer|ECONNRESET|ETIMEDOUT|i/o timeout|TLS handshake timeout|could not resolve host|temporary failure in name resolution|502 Bad Gateway|503 Service Unavailable|504 Gateway|API rate limit exceeded|secondary rate limit|unexpected EOF|failed to download action|no space left on device|The hosted runner encountered an error|Received request to deprovision)`)
	goTestRe   = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	goFileRe   = regexp.MustCompile(`^\s*(\S+\.go):(\d+)(:\d+)?: (.*)$`)
	lintRe     = regexp.MustCompile(`(?i)(golangci-lint|gofmt|go vet|eslint|prettier|ruff|flake8|black would reformat|staticcheck)`)
	permRe     = regexp.MustCompile(`(?i)(Resource not accessible by integration|Permission to .* denied|403 Forbidden)`)
	secretRe   = regexp.MustCompile(`(?i)(Input required and not supplied: (\S+)|Bad credentials|secret \S+ (is )?not (set|found))`)
	actionRe   = regexp.MustCompile(`(?i)(Unable to resolve action [^\s,]+|Can't find 'action\.ya?ml'|Node\.js \d+ actions are deprecated|is not a valid action)`)
	depsRe     = regexp.MustCompile(`(?i)(dependabot|npm ERR! (code )?ERESOLVE|Could not find a version that satisfies|No matching version found|missing go\.sum entry|go: updates to go\.mod needed)`)
	vulnHeadRe = regexp.MustCompile(`^Vulnerability #\d+: (\S+)`)
	vulnFound  = regexp.MustCompile(`^\s*Found in: (\S+)@(\S+)`)
	vulnFixed  = regexp.MustCompile(`^\s*Fixed in: (\S+)@(\S+)`)
)

// keyLines picks the lines that explain a failure, de-duplicated.
func keyLines(lines []string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lines {
		if !errLineRe.MatchString(l) || exitRe.MatchString(l) {
			continue
		}
		l = strings.TrimSpace(strings.TrimPrefix(l, "##[error]"))
		if r := []rune(l); len(r) > 200 {
			l = string(r[:199]) + "…"
		}
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
		if len(out) == limit {
			break
		}
	}
	return out
}

type vuln struct {
	ID, Module, Found, Fixed string
}

func parseVulns(lines []string) []vuln {
	var out []vuln
	cur := -1
	for _, l := range lines {
		if m := vulnHeadRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			out = append(out, vuln{ID: m[1]})
			cur = len(out) - 1
			continue
		}
		if cur < 0 {
			continue
		}
		if m := vulnFound.FindStringSubmatch(l); m != nil && out[cur].Module == "" {
			out[cur].Module, out[cur].Found = m[1], m[2]
		}
		if m := vulnFixed.FindStringSubmatch(l); m != nil && out[cur].Fixed == "" {
			out[cur].Fixed = m[2]
		}
	}
	return out
}

// isStdlib: standard-library paths have no dot in their first element.
func isStdlib(module string) bool {
	first, _, _ := strings.Cut(module, "/")
	return !strings.Contains(first, ".")
}

// Diagnose reads a failed job's log and says why it failed and what would fix it.
func Diagnose(conclusion, jobName string, lines []string) Cause {
	text := strings.Join(lines, "\n")
	key := keyLines(lines, 8)

	if vs := parseVulns(lines); len(vs) > 0 {
		return diagnoseVulns(vs)
	}
	if m := flakyRe.FindString(text); m != "" {
		return Cause{Kind: KindFlaky, Title: "GitHub/runner hiccup: " + m, Hint: "Nothing wrong with the code; a re-run usually passes.", Lines: key, Fix: Fix{Kind: FixRerun}}
	}
	if conclusion == "timed_out" || conclusion == "cancelled" {
		return Cause{Kind: KindFlaky, Title: "the run was " + strings.ReplaceAll(conclusion, "_", " "), Hint: "Re-run it; if it times out again, look for a hanging step or raise timeout-minutes.", Lines: key, Fix: Fix{Kind: FixRerun}}
	}
	if conclusion == "startup_failure" {
		return Cause{Kind: KindConfig, Title: "the workflow file could not start", Hint: "The workflow YAML is invalid or uses something not allowed; open the run page to see GitHub's message.", Lines: key}
	}
	if m := actionRe.FindString(text); m != "" {
		return Cause{Kind: KindConfig, Title: m, Hint: "Pin the action to a version that exists (check its releases) and update deprecated ones.", Lines: key}
	}
	if m := permRe.FindString(text); m != "" {
		return Cause{Kind: KindConfig, Title: m, Hint: "Give the workflow the permission it needs, e.g. `permissions: contents: write` at the top of the workflow.", Lines: key}
	}
	if m := secretRe.FindStringSubmatch(text); m != nil {
		return Cause{Kind: KindConfig, Title: m[0], Hint: "A secret or input is missing or wrong: Settings → Secrets and variables → Actions.", Lines: key}
	}
	var tests []string
	for _, l := range lines {
		if m := goTestRe.FindStringSubmatch(l); m != nil {
			tests = append(tests, m[1])
		}
	}
	if len(tests) > 0 {
		return Cause{Kind: KindTest, Title: "failing tests: " + strings.Join(uniq(tests), ", "), Hint: "Run them locally: go test -run '" + strings.Join(uniq(tests), "|") + "' ./...", Lines: key}
	}
	if depsRe.MatchString(text) || strings.Contains(strings.ToLower(jobName), "dependabot") || strings.Contains(strings.ToLower(jobName), "graph") {
		title := "dependency install failed"
		for _, l := range lines {
			if i := strings.Index(l, `"message": "`); i >= 0 {
				msg := l[i+12:]
				if j := strings.Index(msg, `\\n`); j >= 0 {
					msg = msg[:j]
				}
				title = strings.TrimSuffix(firstSentence(strings.ReplaceAll(msg, `\"`, `"`)), `"`)
				break
			}
		}
		return Cause{Kind: KindDeps, Title: title, Hint: "A dependency file can't be read or resolved; fix the line it names (requirements.txt, go.mod, package.json…).", Lines: key}
	}
	if lintRe.MatchString(jobName+"\n"+text) && !goFileRe.MatchString(text) {
		return Cause{Kind: KindLint, Title: "lint/format check failed", Hint: "Run the same linter or formatter locally and commit its changes.", Lines: key}
	}
	for _, l := range lines {
		if m := goFileRe.FindStringSubmatch(l); m != nil {
			kind := KindBuild
			if lintRe.MatchString(jobName) {
				kind = KindLint
			}
			return Cause{Kind: kind, Title: m[1] + ":" + m[2] + ": " + m[4], Hint: "Fix the code at that line; reproduce with go build ./... && go vet ./...", Lines: key}
		}
	}
	title := "failed for a reason gh compact does not recognise"
	if len(key) > 0 {
		title = key[0]
	}
	return Cause{Kind: KindUnknown, Title: title, Hint: "Read the lines below or the run page. A re-run is safe but may fail again.", Lines: key}
}

func diagnoseVulns(vs []vuln) Cause {
	fix := Fix{Modules: map[string]string{}}
	var lines, manual []string
	for _, v := range vs {
		fix.IDs = append(fix.IDs, v.ID)
		line := v.ID + " in " + v.Module + "@" + v.Found
		if v.Fixed == "" || strings.EqualFold(v.Fixed, "N/A") {
			manual = append(manual, v.ID)
			lines = append(lines, line+" (no fixed version yet)")
			continue
		}
		lines = append(lines, line+" → fixed in "+v.Fixed)
		if isStdlib(v.Module) {
			if goVersionLess(fix.Toolchain, v.Fixed) {
				fix.Toolchain = v.Fixed
			}
		} else if goVersionLess(fix.Modules[v.Module], v.Fixed) {
			fix.Modules[v.Module] = v.Fixed
		}
	}
	fix.IDs = uniq(fix.IDs)
	c := Cause{Kind: KindVuln, Lines: uniq(lines)}
	parts := []string{}
	if fix.Toolchain != "" {
		parts = append(parts, "Go toolchain → "+fix.Toolchain)
	}
	mods := make([]string, 0, len(fix.Modules))
	for m, v := range fix.Modules {
		mods = append(mods, m+"@"+v)
	}
	sort.Strings(mods)
	parts = append(parts, mods...)
	c.Title = strconv.Itoa(len(fix.IDs)) + " known " + plural(len(fix.IDs), "vulnerability", "vulnerabilities") + " (" + strings.Join(fix.IDs, ", ") + ")"
	switch {
	case len(parts) == 0:
		c.Hint = "No fixed version is published yet; wait for one or stop calling the affected code."
	case fix.Toolchain != "" && len(fix.Modules) == 0:
		fix.Kind = FixToolchain
		c.Hint = "Bump " + strings.Join(parts, ", ") + " in go.mod."
	default:
		fix.Kind = FixGoGet
		c.Hint = "Update " + strings.Join(parts, ", ") + "."
	}
	if len(manual) > 0 && len(parts) > 0 {
		c.Hint += " Still unfixed upstream: " + strings.Join(manual, ", ") + "."
	}
	c.Fix = fix
	return c
}

var goVerRe = regexp.MustCompile(`\d+`)

// goVersionLess compares go1.27.3 / v1.2.10 style versions numerically; "" is lowest.
func goVersionLess(a, b string) bool {
	if a == "" {
		return b != ""
	}
	na, nb := goVerRe.FindAllString(a, -1), goVerRe.FindAllString(b, -1)
	for i := 0; i < len(na) || i < len(nb); i++ {
		var x, y int
		if i < len(na) {
			x, _ = strconv.Atoi(na[i])
		}
		if i < len(nb) {
			y, _ = strconv.Atoi(nb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

var (
	toolchainLine = regexp.MustCompile(`(?m)^toolchain[ \t]+(\S+)[ \t]*$`)
	goLine        = regexp.MustCompile(`(?m)^go[ \t]+(\S+)[ \t]*$`)
)

// bumpToolchain rewrites go.mod so CI (setup-go with go-version-file) installs
// at least want. It returns the new file and the version it replaced.
func bumpToolchain(gomod, want string) (string, string, bool) {
	if m := toolchainLine.FindStringSubmatchIndex(gomod); m != nil {
		old := gomod[m[2]:m[3]]
		if !goVersionLess(old, want) {
			return gomod, old, false
		}
		return gomod[:m[2]] + want + gomod[m[3]:], old, true
	}
	m := goLine.FindStringSubmatchIndex(gomod)
	if m == nil {
		return gomod, "", false
	}
	old := "go" + gomod[m[2]:m[3]]
	if !goVersionLess(old, want) {
		return gomod, old, false
	}
	return gomod[:m[1]] + "\n\ntoolchain " + want + gomod[m[1]:], old, true
}
