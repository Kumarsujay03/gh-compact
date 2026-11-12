package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// fakeGitHub keeps one repo's releases and tags in memory.
type fakeGitHub struct {
	mu     sync.Mutex
	rels   map[int64]*Release
	tags   map[string]string
	nextID int64
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/")
	const base = "repos/me/app/"
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	switch {
	case p == "user":
		reply(200, map[string]string{"login": "me"})
	case p == "repos/me/app" && r.Method == "GET":
		reply(200, map[string]any{"full_name": "me/app", "permissions": map[string]bool{"push": true}})
	case p == base+"releases" && r.Method == "GET":
		var out []Release
		if r.URL.Query().Get("page") == "1" {
			for _, rel := range f.rels {
				out = append(out, *rel)
			}
			sort.Slice(out, func(i, j int) bool { return out[i].When().After(out[j].When()) })
		}
		reply(200, out)
	case p == base+"releases" && r.Method == "POST":
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.nextID++
		rel := &Release{ID: f.nextID, TagName: in["tag_name"].(string), Name: in["name"].(string), Body: in["body"].(string), PublishedAt: time.Now()}
		f.rels[rel.ID] = rel
		reply(201, rel)
	case strings.HasPrefix(p, base+"releases/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(p, base+"releases/"), 10, 64)
		rel, ok := f.rels[id]
		if !ok {
			reply(404, map[string]string{"message": "Not Found"})
			return
		}
		switch r.Method {
		case "PATCH":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			rel.Body = in["body"]
			reply(200, rel)
		case "DELETE":
			delete(f.rels, id)
			w.WriteHeader(204)
		}
	case strings.HasPrefix(p, base+"git/ref/tags/"):
		tag, _ := url.PathUnescape(strings.TrimPrefix(p, base+"git/ref/tags/"))
		reply(200, map[string]any{"object": map[string]string{"sha": f.tags[tag]}})
	case strings.HasPrefix(p, base+"git/refs/tags/") && r.Method == "DELETE":
		tag, _ := url.PathUnescape(strings.TrimPrefix(p, base+"git/refs/tags/"))
		delete(f.tags, tag)
		w.WriteHeader(204)
	case p == base+"git/refs" && r.Method == "POST":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.tags[strings.TrimPrefix(in["ref"], "refs/tags/")] = in["sha"]
		reply(201, in)
	default:
		reply(404, map[string]string{"message": "unhandled " + r.Method + " " + p})
	}
}

type toServer struct{ target *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.target.Scheme, t.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

func TestCompactAndRestoreAgainstFakeGitHub(t *testing.T) {
	f := &fakeGitHub{rels: map[int64]*Release{}, tags: map[string]string{}}
	start := time.Now().Add(-48 * time.Hour)
	for i := int64(1); i <= 6; i++ {
		tag := fmt.Sprintf("v0.%d", i)
		f.rels[i] = &Release{ID: i, TagName: tag, PublishedAt: start.Add(time.Duration(i) * 10 * time.Minute),
			Body: "- change " + tag, Assets: []Asset{{Name: "app.tar.gz", Size: 2048}}}
		f.tags[tag] = "sha-" + tag
	}
	f.nextID = 100
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	cl, err := NewClient(api.ClientOptions{Host: "github.com", AuthToken: "test", Transport: toServer{u}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_COMPACT_BACKUP_DIR", t.TempDir())

	repo, err := cl.Repo("me/app")
	if err != nil {
		t.Fatal(err)
	}
	o := PlanOptions{Strategy: ByBurst, Gap: time.Hour, MaxGroup: 3, MinReleases: 3, MinAge: time.Hour, Now: time.Now()}
	plans := scan(cl, []Repo{repo}, o)
	if plans[0].Err != nil || len(plans[0].Groups) != 2 {
		t.Fatalf("plan: %+v", plans[0])
	}
	if err := apply(cl, plans, true); err != nil {
		t.Fatal(err)
	}

	left, _ := cl.Releases("me/app")
	var got []string
	for _, r := range left {
		got = append(got, r.TagName)
	}
	if strings.Join(got, ",") != "v0.6,v0.3" {
		t.Fatalf("left: %v", got)
	}
	if !strings.Contains(left[0].Body, "<b>v0.4</b>") || !strings.Contains(left[0].Body, "- change v0.5") {
		t.Fatalf("v0.6 notes do not carry v0.4/v0.5:\n%s", left[0].Body)
	}
	if _, ok := f.tags["v0.4"]; ok {
		t.Fatal("--delete-tags should have removed tag v0.4")
	}

	path, err := latestBackup()
	if err != nil {
		t.Fatal(err)
	}
	b, err := readBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreBackup(cl, b); err != nil {
		t.Fatal(err)
	}
	left, _ = cl.Releases("me/app")
	if len(left) != 6 {
		t.Fatalf("restore brought back %d releases, want 6", len(left))
	}
	for _, r := range left {
		if r.Body != "- change "+r.TagName {
			t.Errorf("%s body after restore: %q", r.TagName, r.Body)
		}
	}
	if f.tags["v0.4"] != "sha-v0.4" {
		t.Errorf("tag v0.4 restored at %q", f.tags["v0.4"])
	}
}
