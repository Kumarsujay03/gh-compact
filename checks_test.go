package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// fakeActions serves one repo with a failing govulncheck run and a flaky test run.
type fakeActions struct {
	mu        sync.Mutex
	gomod     string
	commits   []string
	reran     bool
	newCommit string
}

func (f *fakeActions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/repos/me/app/")
	reply := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	old := time.Now().Add(-time.Hour)
	vulnRun := Run{ID: 1, Name: "govulncheck", WorkflowID: 7, Status: "completed", Conclusion: "failure", CreatedAt: old, HeadBranch: "main"}
	testRun := Run{ID: 2, Name: "ci", WorkflowID: 8, Status: "completed", Conclusion: "failure", CreatedAt: old, HeadBranch: "main"}
	okRun := Run{ID: 3, Name: "ci", WorkflowID: 8, Status: "completed", Conclusion: "success", CreatedAt: old.Add(-time.Hour)}
	switch {
	case p == "actions/runs" && r.URL.Query().Get("head_sha") != "":
		var runs []Run
		if r.URL.Query().Get("head_sha") == f.newCommit {
			runs = []Run{{ID: 9, WorkflowID: 7, Status: "completed", Conclusion: "success"}}
		}
		reply(map[string]any{"workflow_runs": runs})
	case p == "actions/runs":
		reply(map[string]any{"workflow_runs": []Run{vulnRun, testRun, okRun}})
	case p == "actions/runs/1/jobs":
		reply(map[string]any{"jobs": []Job{{ID: 11, Name: "vuln", Conclusion: "failure"}}})
	case p == "actions/runs/2/jobs":
		reply(map[string]any{"jobs": []Job{{ID: 21, Name: "test", Conclusion: "failure"}, {ID: 22, Name: "lint", Conclusion: "success"}}})
	case p == "actions/jobs/11/logs":
		_, _ = w.Write([]byte(vulnLog))
	case p == "actions/jobs/21/logs":
		_, _ = w.Write([]byte("2026-11-12T10:00:00Z ##[error]The runner has received a shutdown signal.\n"))
	case p == "actions/runs/2/rerun-failed-jobs":
		f.reran = true
		w.WriteHeader(201)
	case p == "actions/runs/2":
		run := testRun
		if f.reran {
			run.Conclusion = "success"
		}
		reply(run)
	case p == "contents/go.mod" && r.Method == "GET":
		reply(map[string]string{"sha": "blob1", "content": base64.StdEncoding.EncodeToString([]byte(f.gomod))})
	case p == "contents/go.mod" && r.Method == "PUT":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["sha"] != "blob1" || in["branch"] != "main" {
			w.WriteHeader(409)
			return
		}
		b, _ := base64.StdEncoding.DecodeString(in["content"].(string))
		f.gomod = string(b)
		f.commits = append(f.commits, in["message"].(string))
		f.newCommit = "c0ffee"
		reply(map[string]any{"commit": map[string]string{"sha": f.newCommit}})
	default:
		w.WriteHeader(404)
		reply(map[string]string{"message": "unhandled " + r.Method + " " + r.URL.Path})
	}
}

func TestChecksDiagnoseFixAndWait(t *testing.T) {
	f := &fakeActions{gomod: "module app\n\ngo 1.27.2\n\ntoolchain go1.27.3\n"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	cl, err := NewClient(api.ClientOptions{Host: "github.com", AuthToken: "test", Transport: toServer{u}})
	if err != nil {
		t.Fatal(err)
	}
	pollEvery = 10 * time.Millisecond
	repo := Repo{FullName: "me/app", Default: "main"}
	repo.Permissions.Push = true
	c := checksCLI{since: 24 * time.Hour, timeout: 5 * time.Second}

	fails := checkRepo(cl, repo, c)
	if len(fails) != 2 {
		t.Fatalf("want 2 failing workflows, got %d", len(fails))
	}
	byName := map[string]*Failing{}
	for _, x := range fails {
		byName[x.Run.Name] = x
	}
	v, ci := byName["govulncheck"], byName["ci"]
	if v.Cause.Kind != KindVuln || v.Change == nil || !strings.Contains(v.Change.Desc, "go1.27.3 → go1.27.4") {
		t.Fatalf("vuln: %+v %+v", v.Cause, v.Change)
	}
	if ci.Cause.Kind != KindFlaky || !ci.fixable(c) || len(ci.Jobs) != 1 {
		t.Fatalf("ci: %+v jobs %v", ci.Cause, ci.Jobs)
	}

	if err := fixChecks(cl, fails, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.gomod, "toolchain go1.27.4") || len(f.commits) != 1 || !strings.Contains(f.commits[0], "GO-2026-6859") {
		t.Fatalf("go.mod not fixed: %q commits %q", f.gomod, f.commits)
	}
	if !f.reran {
		t.Fatal("flaky run was not re-run")
	}
}
