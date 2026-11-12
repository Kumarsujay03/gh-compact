package main

import (
	"strings"
	"testing"
)

func lines(s string) []string { return cleanLog(s) }

// Trimmed from a real govulncheck job log.
const vulnLog = "2026-11-12T15:36:26.7460211Z ##[group]Run govulncheck ./...\n" +
	"2026-11-12T15:36:32.1924808Z Vulnerability #1: GO-2026-6859\n" +
	"2026-11-12T15:36:32.1924900Z     HTTP/1 server crash due to MaxBytesReader header race in net/http\n" +
	"2026-11-12T15:36:32.1925679Z   More info: https://pkg.go.dev/vuln/GO-2026-6859\n" +
	"2026-11-12T15:36:32.1925700Z   Standard library\n" +
	"2026-11-12T15:36:32.1926250Z     Found in: net/http@go1.27.3\n" +
	"2026-11-12T15:36:32.1926300Z     Fixed in: net/http@go1.27.4\n" +
	"2026-11-12T15:36:32.1944836Z ##[error]      #1: internal/p2p/p2p.go:331:31: p2p.connect calls http.CanonicalHeaderKey\n" +
	"2026-11-12T15:36:32.2000000Z ##[error]Process completed with exit code 3.\n"

func TestDiagnoseStdlibVuln(t *testing.T) {
	c := Diagnose("failure", "vuln", lines(vulnLog))
	if c.Kind != KindVuln || c.Fix.Kind != FixToolchain || c.Fix.Toolchain != "go1.27.4" {
		t.Fatalf("got %+v", c)
	}
	if !strings.Contains(c.Title, "GO-2026-6859") {
		t.Errorf("title: %s", c.Title)
	}
}

func TestDiagnoseModuleVuln(t *testing.T) {
	log := `Vulnerability #1: GO-2026-0001
  Found in: golang.org/x/net@v0.30.0
  Fixed in: golang.org/x/net@v0.33.0
Vulnerability #2: GO-2026-0002
  Found in: golang.org/x/net@v0.30.0
  Fixed in: golang.org/x/net@v0.31.0
Vulnerability #3: GO-2026-0003
  Found in: crypto/tls@go1.27.1
  Fixed in: crypto/tls@go1.27.2
Vulnerability #4: GO-2026-0004
  Found in: example.com/old@v1.0.0
  Fixed in: N/A`
	c := Diagnose("failure", "vuln", lines(log))
	if c.Fix.Kind != FixGoGet || c.Fix.Modules["golang.org/x/net"] != "v0.33.0" || c.Fix.Toolchain != "go1.27.2" {
		t.Fatalf("got %+v", c.Fix)
	}
	if !strings.Contains(c.Hint, "GO-2026-0004") {
		t.Errorf("unfixable vuln should be named: %s", c.Hint)
	}
}

func TestDiagnoseKinds(t *testing.T) {
	cases := []struct {
		name, concl, job, log, kind, fix string
	}{
		{"runner", "failure", "build", "##[error]The runner has received a shutdown signal.", KindFlaky, FixRerun},
		{"network", "failure", "test", "go: downloading x\ndial tcp: lookup proxy.golang.org: i/o timeout", KindFlaky, FixRerun},
		{"timeout", "timed_out", "test", "", KindFlaky, FixRerun},
		{"gotest", "failure", "test", "--- FAIL: TestParse (0.00s)\n    parse_test.go:12: got 1 want 2\nFAIL\tpkg\t0.1s", KindTest, ""},
		{"build", "failure", "build", "# pkg\n./main.go:10:2: undefined: foo", KindBuild, ""},
		{"lint", "failure", "lint", "Run golangci-lint\nmain.go:3:1: exported func Foo should have comment (revive)", KindLint, ""},
		{"perm", "failure", "release", "##[error]Resource not accessible by integration", KindConfig, ""},
		{"secret", "failure", "deploy", "##[error]Input required and not supplied: token", KindConfig, ""},
		{"action", "failure", "ci", "##[error]Unable to resolve action actions/checkout@v99, unable to find version v99", KindConfig, ""},
		{"startup", "startup_failure", "ci", "", KindConfig, ""},
		{"dependabot", "failure", "update-pip-graph", `Handled error  "message": "InstallationError(\"Invalid requirement: 'Version 0.01'\\n more"`, KindDeps, ""},
		{"unknown", "failure", "x", "something odd\n##[error]Process completed with exit code 1.", KindUnknown, ""},
	}
	for _, tc := range cases {
		c := Diagnose(tc.concl, tc.job, lines(tc.log))
		if c.Kind != tc.kind || c.Fix.Kind != tc.fix {
			t.Errorf("%s: got kind %q fix %q (%s)", tc.name, c.Kind, c.Fix.Kind, c.Title)
		}
	}
	c := Diagnose("failure", "update-pip-graph", lines(cases[10].log))
	if strings.Contains(c.Title, `\n`) || !strings.Contains(c.Title, "Version 0.01") {
		t.Errorf("dependabot title: %q", c.Title)
	}
	c = Diagnose("failure", "test", lines(cases[3].log))
	if !strings.Contains(c.Hint, "-run 'TestParse'") {
		t.Errorf("test hint: %q", c.Hint)
	}
}

func TestBumpToolchain(t *testing.T) {
	in := "module x\n\ngo 1.27.2\n\ntoolchain go1.27.3\n\nrequire a v1\n"
	out, old, ok := bumpToolchain(in, "go1.27.4")
	if !ok || old != "go1.27.3" || !strings.Contains(out, "toolchain go1.27.4\n") || !strings.Contains(out, "require a v1") {
		t.Fatalf("%v %q\n%s", ok, old, out)
	}
	if _, _, ok := bumpToolchain(out, "go1.27.4"); ok {
		t.Error("already new enough should not change")
	}
	out, old, ok = bumpToolchain("module x\n\ngo 1.26.1\n", "go1.26.10")
	if !ok || old != "go1.26.1" || !strings.Contains(out, "go 1.26.1\n\ntoolchain go1.26.10") {
		t.Fatalf("no toolchain line: %v %q\n%s", ok, old, out)
	}
	if !goVersionLess("go1.27.9", "go1.27.10") || goVersionLess("v0.33.0", "v0.31.0") {
		t.Error("version compare")
	}
}

func TestBestCausePrefersFixable(t *testing.T) {
	c := bestCause([]Cause{{Kind: KindUnknown}, {Kind: KindFlaky, Fix: Fix{Kind: FixRerun}}, {Kind: KindVuln, Fix: Fix{Kind: FixToolchain}}})
	if c.Kind != KindVuln {
		t.Fatalf("got %s", c.Kind)
	}
}
