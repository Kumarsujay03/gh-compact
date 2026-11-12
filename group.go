package main

import (
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Strategy names for --by.
const (
	ByBurst = "burst" // releases that came out close together
	ByMinor = "minor" // all patch releases of one major.minor
	ByCount = "count" // fixed-size chunks, newest first
)

type PlanOptions struct {
	Strategy    string
	Gap         time.Duration // burst: max time between two neighbouring releases
	MaxGroup    int           // most releases folded into one (<=0: no limit)
	MinReleases int           // repos with fewer eligible releases are left alone
	Keep        int           // newest N releases are never touched
	MinAge      time.Duration // releases younger than this are left alone
	Prereleases bool          // also fold prereleases
	Now         time.Time
}

// Group is one compaction: Survivor (the newest) absorbs Members, which are deleted.
type Group struct {
	Survivor Release
	Members  []Release // newest first
}

func (g Group) Oldest() Release { return g.Members[len(g.Members)-1] }

func (g Group) Span() time.Duration { return g.Survivor.When().Sub(g.Oldest().When()) }

// DroppedAssets counts the files attached to releases that will be deleted.
func (g Group) DroppedAssets() (n int, bytes int64) {
	for _, m := range g.Members {
		for _, a := range m.Assets {
			n++
			bytes += a.Size
		}
	}
	return
}

// Eligible returns the releases a plan may touch, newest first.
func Eligible(rels []Release, o PlanOptions) []Release {
	var out []Release
	for _, r := range rels {
		if r.Draft || (r.Prerelease && !o.Prereleases) {
			continue
		}
		if o.MinAge > 0 && o.Now.Sub(r.When()) < o.MinAge {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].When().After(out[j].When()) })
	if o.Keep > 0 {
		if o.Keep >= len(out) {
			return nil
		}
		out = out[o.Keep:]
	}
	return out
}

// Plan decides which releases fold into which.
func Plan(rels []Release, o PlanOptions) ([]Group, error) {
	el := Eligible(rels, o)
	if len(el) < max(2, o.MinReleases) {
		return nil, nil
	}
	var same func(group []Release, next Release) bool
	switch o.Strategy {
	case ByBurst, "":
		same = func(g []Release, next Release) bool {
			return g[len(g)-1].When().Sub(next.When()) <= o.Gap
		}
	case ByMinor:
		same = func(g []Release, next Release) bool {
			a, ok1 := minorKey(g[0].TagName)
			b, ok2 := minorKey(next.TagName)
			return ok1 && ok2 && a == b
		}
	case ByCount:
		same = func([]Release, Release) bool { return true }
	default:
		return nil, fmt.Errorf("unknown --by %q (use burst, minor or count)", o.Strategy)
	}

	var groups []Group
	cur, size := []Release{el[0]}, weight(el[0])
	flush := func() {
		if len(cur) >= 2 {
			groups = append(groups, Group{Survivor: cur[0], Members: append([]Release(nil), cur[1:]...)})
		}
	}
	for _, r := range el[1:] {
		if (o.MaxGroup <= 0 || size+weight(r) <= o.MaxGroup) && same(cur, r) {
			cur, size = append(cur, r), size+weight(r)
			continue
		}
		flush()
		cur, size = []Release{r}, weight(r)
	}
	flush()
	return groups, nil
}

// weight is how many releases one release already stands for: itself plus
// any gh-compact folded into it before. --max-group caps the total, so
// running gh compact again never folds full releases into each other.
func weight(r Release) int {
	_, folded := splitBody(r.Body)
	return 1 + len(folded)
}

var semverRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// minorKey turns "v1.4.2" into "v1.4" and "app-1.4.2" into "app-1.4", so
// two different prefixes in one repo never fold together.
func minorKey(tag string) (string, bool) {
	loc := semverRe.FindStringSubmatchIndex(tag)
	if loc == nil {
		return "", false
	}
	return tag[:loc[0]] + tag[loc[2]:loc[3]] + "." + tag[loc[4]:loc[5]], true
}
