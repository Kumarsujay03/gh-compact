package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func rel(id int64, tag string, at time.Time) Release {
	return Release{ID: id, TagName: tag, PublishedAt: at, Body: "- change in " + tag}
}

func tags(g Group) string {
	s := []string{g.Survivor.TagName + " <"}
	for _, m := range g.Members {
		s = append(s, m.TagName)
	}
	return strings.Join(s, " ")
}

func opts() PlanOptions {
	return PlanOptions{Strategy: ByBurst, Gap: 24 * time.Hour, MaxGroup: 4, MinReleases: 3, Now: t0.Add(30 * 24 * time.Hour)}
}

func TestBurstSplitsByGapAndSize(t *testing.T) {
	var rels []Release
	// eight releases ten minutes apart, like a CI that tags every push
	for i := 0; i < 8; i++ {
		rels = append(rels, rel(int64(i), fmt.Sprintf("v0.1.%d", 10+i), t0.Add(time.Duration(i)*10*time.Minute)))
	}
	// a lone release a week earlier stays alone
	rels = append(rels, rel(99, "v0.1.0", t0.Add(-7*24*time.Hour)))
	groups, err := Plan(rels, opts())
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, g := range groups {
		got = append(got, tags(g))
	}
	want := []string{"v0.1.17 < v0.1.16 v0.1.15 v0.1.14", "v0.1.13 < v0.1.12 v0.1.11 v0.1.10"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestNoLimitAndGap(t *testing.T) {
	rels := []Release{
		rel(1, "v3", t0.Add(4*24*time.Hour)),
		rel(2, "v2", t0.Add(2*24*time.Hour)),
		rel(3, "v1", t0),
	}
	o := opts()
	if g, _ := Plan(rels, o); len(g) != 0 {
		t.Fatalf("2-day gaps with a 1d window should not fold, got %v", g)
	}
	o.Gap, o.MaxGroup = 3*24*time.Hour, 0
	g, _ := Plan(rels, o)
	if len(g) != 1 || tags(g[0]) != "v3 < v2 v1" {
		t.Fatalf("got %v", g)
	}
}

func TestSkipsDraftsPrereleasesYoungAndKept(t *testing.T) {
	now := t0.Add(5 * time.Hour)
	rels := []Release{
		rel(1, "v1.0.5", now.Add(-10*time.Minute)), // too young
		rel(2, "v1.0.4", t0.Add(4*time.Hour)),      // kept by --keep 1
		rel(3, "v1.0.3", t0.Add(3*time.Hour)),
		{ID: 4, TagName: "v1.0.3-rc1", PublishedAt: t0.Add(2 * time.Hour), Prerelease: true},
		{ID: 5, TagName: "draft", Draft: true},
		rel(6, "v1.0.2", t0.Add(1*time.Hour)),
		rel(7, "v1.0.1", t0),
	}
	o := opts()
	o.Now, o.MinAge, o.Keep = now, time.Hour, 1
	g, _ := Plan(rels, o)
	if len(g) != 1 || tags(g[0]) != "v1.0.3 < v1.0.2 v1.0.1" {
		t.Fatalf("got %v", g)
	}
}

func TestMinReleases(t *testing.T) {
	rels := []Release{rel(1, "v2", t0.Add(time.Hour)), rel(2, "v1", t0)}
	if g, _ := Plan(rels, opts()); len(g) != 0 {
		t.Fatalf("a repo with 2 releases is under --min-releases 3, got %v", g)
	}
}

func TestByMinor(t *testing.T) {
	day := 24 * time.Hour
	rels := []Release{
		rel(1, "v1.5.0", t0.Add(50*day)),
		rel(2, "v1.4.2", t0.Add(40*day)),
		rel(3, "v1.4.1", t0.Add(20*day)),
		rel(4, "v1.4.0", t0.Add(10*day)),
		rel(5, "v1.3.0", t0),
	}
	o := opts()
	o.Strategy = ByMinor
	g, _ := Plan(rels, o)
	if len(g) != 1 || tags(g[0]) != "v1.4.2 < v1.4.1 v1.4.0" {
		t.Fatalf("got %v", g)
	}
}

func TestByCount(t *testing.T) {
	var rels []Release
	for i := 0; i < 5; i++ {
		rels = append(rels, rel(int64(i), fmt.Sprintf("v%d", i), t0.Add(time.Duration(i)*30*24*time.Hour)))
	}
	o := opts()
	o.Strategy, o.MaxGroup = ByCount, 2
	g, _ := Plan(rels, o)
	if len(g) != 2 || tags(g[0]) != "v4 < v3" || tags(g[1]) != "v2 < v1" {
		t.Fatalf("got %v", g)
	}
}

func TestMinorKey(t *testing.T) {
	for tag, want := range map[string]string{"v1.4.2": "v1.4", "app-2.0.1": "app-2.0", "1.2": "1.2"} {
		if got, ok := minorKey(tag); !ok || got != want {
			t.Errorf("minorKey(%q) = %q, want %q", tag, got, want)
		}
	}
	if _, ok := minorKey("nightly"); ok {
		t.Error("nightly has no version")
	}
}

func TestDurationFlag(t *testing.T) {
	var d time.Duration
	v := durationValue{&d}
	for in, want := range map[string]time.Duration{"3d": 72 * time.Hour, "2w": 14 * 24 * time.Hour, "90m": 90 * time.Minute, "1.5d": 36 * time.Hour} {
		if err := v.Set(in); err != nil || d != want {
			t.Errorf("%s: got %v %v", in, d, err)
		}
	}
	if v.Set("soon") == nil {
		t.Error("expected an error")
	}
}

func TestRerunAfterCompactIsStable(t *testing.T) {
	var rels []Release
	for i := 0; i < 8; i++ {
		rels = append(rels, rel(int64(i), fmt.Sprintf("v0.1.%d", 10+i), t0.Add(time.Duration(i)*10*time.Minute)))
	}
	groups, _ := Plan(rels, opts())
	var left []Release
	for _, g := range groups {
		s := g.Survivor
		s.Body = MergeNotes(g)
		left = append(left, s)
	}
	if again, _ := Plan(left, PlanOptions{Strategy: ByBurst, Gap: 24 * time.Hour, MaxGroup: 4, Now: opts().Now}); len(again) != 0 {
		t.Fatalf("second run folded full releases: %v", again)
	}
	// with no cap they may still merge into one
	again, _ := Plan(left, PlanOptions{Strategy: ByBurst, Gap: 24 * time.Hour, MaxGroup: 0, Now: opts().Now})
	if len(again) != 1 || len(FoldedEntries(again[0])) != 7 {
		t.Fatalf("uncapped re-run should fold all 7 older notes: %v", again)
	}
}
