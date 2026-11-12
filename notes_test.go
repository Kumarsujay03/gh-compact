package main

import (
	"strings"
	"testing"
	"time"
)

func TestMergeKeepsEverything(t *testing.T) {
	g := Group{
		Survivor: Release{TagName: "v1.3.3", PublishedAt: t0.Add(3 * time.Hour), Body: "- `improve` can now teach dim light.\n  Below 0.35 it still refuses."},
		Members: []Release{
			{TagName: "v1.3.2", PublishedAt: t0.Add(2 * time.Hour), Body: "## Fixes\n- Someone else's failed tries no longer lock the owner out. Long detail here.\n  more\n"},
			{TagName: "v1.3.1", PublishedAt: t0.Add(1 * time.Hour), Body: "* feat: new `keyring` check by @someone in https://github.com/a/b/pull/1\n* Fixed: `status` crashed."},
		},
	}
	body := MergeNotes(g)
	for _, want := range []string{
		"- `improve` can now teach dim light.", // survivor's own notes stay on top
		"`v1.3.1` → `v1.3.2`",
		"**✨ New**\n- new `keyring` check\n",
		"**🐛 Fixed**\n- Someone else's failed tries no longer lock the owner out.\n- Fixed: `status` crashed.",
		"<b>v1.3.2</b> · 2026-11-01",
		"Long detail here.", // full notes kept in the details
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, "- `improve`") {
		t.Errorf("survivor notes should come first:\n%s", body)
	}
}

func TestCompactingTwiceDoesNotNest(t *testing.T) {
	first := Group{
		Survivor: Release{TagName: "v3", PublishedAt: t0.Add(3 * time.Hour), Body: "- three"},
		Members: []Release{
			{TagName: "v2", PublishedAt: t0.Add(2 * time.Hour), Body: "- two"},
			{TagName: "v1", PublishedAt: t0.Add(1 * time.Hour), Body: "- one"},
		},
	}
	v3 := first.Survivor
	v3.Body = MergeNotes(first)

	second := Group{
		Survivor: Release{TagName: "v5", PublishedAt: t0.Add(5 * time.Hour), Body: "- five"},
		Members:  []Release{{TagName: "v4", PublishedAt: t0.Add(4 * time.Hour), Body: "- four"}, v3},
	}
	body := MergeNotes(second)
	if n := strings.Count(body, markStart); n != 1 {
		t.Fatalf("want one generated block, got %d:\n%s", n, body)
	}
	entries := FoldedEntries(second)
	var got []string
	for _, e := range entries {
		got = append(got, e.Tag+"="+e.Body)
	}
	if strings.Join(got, ",") != "v4=- four,v3=- three,v2=- two,v1=- one" {
		t.Fatalf("entries: %v", got)
	}

	// re-merging the survivor of an already-compacted group is stable
	again := second
	again.Survivor.Body = body
	if MergeNotes(again) != body {
		t.Fatal("merging the same group twice changed the notes")
	}
}

func TestProseBodiesAndEmpty(t *testing.T) {
	g := Group{
		Survivor: Release{TagName: "v2", PublishedAt: t0.Add(time.Hour)},
		Members:  []Release{{TagName: "v1", PublishedAt: t0, Body: "This release makes startup twice as fast. It also does more."}},
	}
	body := MergeNotes(g)
	if !strings.Contains(body, "- This release makes startup twice as fast.\n") {
		t.Fatalf("prose summary missing:\n%s", body)
	}
	g.Members[0].Body = ""
	if !strings.Contains(MergeNotes(g), "_No release notes._") {
		t.Fatal("empty notes should say so")
	}
}

func TestSizeLimit(t *testing.T) {
	big := strings.Repeat("x", 50000)
	g := Group{Survivor: Release{TagName: "v9", PublishedAt: t0.Add(9 * time.Hour)}}
	for i := 8; i >= 1; i-- {
		g.Members = append(g.Members, Release{TagName: "v" + string(rune('0'+i)), PublishedAt: t0.Add(time.Duration(i) * time.Hour), Body: big})
	}
	body := MergeNotes(g)
	if len(body) > maxBody {
		t.Fatalf("body is %d chars", len(body))
	}
	if !strings.Contains(body, "oldest releases were left out") {
		t.Fatal("should say notes were trimmed")
	}
}

func TestFirstSentence(t *testing.T) {
	long := strings.Repeat("word ", 60)
	if got := firstSentence(long); len([]rune(got)) > 141 || !strings.HasSuffix(got, "…") {
		t.Errorf("not trimmed: %q", got)
	}
	if got := firstSentence("Bumped to v1.2.3 today. More."); got != "Bumped to v1.2.3 today." {
		t.Errorf("got %q", got)
	}
}
