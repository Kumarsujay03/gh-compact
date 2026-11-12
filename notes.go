package main

import (
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	markStart    = "<!-- gh-compact:start -->"
	markEnd      = "<!-- gh-compact:end -->"
	markEntryEnd = "<!-- gh-compact:entry-end -->"
	// GitHub refuses release bodies over 125,000 characters.
	maxBody = 120000
)

var entryRe = regexp.MustCompile(`(?s)<!-- gh-compact:entry tag="([^"]*)" date="([^"]*)" -->\n(.*?)\n?` + regexp.QuoteMeta(markEntryEnd))

// Entry is one release's own notes.
type Entry struct {
	Tag  string
	Date time.Time
	Body string
}

// splitBody separates a release's own notes from notes gh-compact folded in
// earlier, so compacting twice never nests or loses anything.
func splitBody(body string) (own string, folded []Entry) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	start := strings.Index(body, markStart)
	if start < 0 {
		return strings.TrimSpace(body), nil
	}
	rest := body[start+len(markStart):]
	block, after := rest, ""
	if end := strings.Index(rest, markEnd); end >= 0 {
		block, after = rest[:end], rest[end+len(markEnd):]
	}
	own = strings.TrimSpace(body[:start])
	if a := strings.TrimSpace(after); a != "" {
		own = strings.TrimSpace(own + "\n\n" + a)
	}
	for _, m := range entryRe.FindAllStringSubmatch(block, -1) {
		t, _ := time.Parse(time.RFC3339, m[2])
		folded = append(folded, Entry{Tag: html.UnescapeString(m[1]), Date: t, Body: strings.TrimSpace(m[3])})
	}
	return strings.TrimSpace(own), folded
}

// FoldedEntries lists every older release whose notes end up in the survivor,
// newest first, without duplicates.
func FoldedEntries(g Group) []Entry {
	_, entries := splitBody(g.Survivor.Body)
	for _, m := range g.Members {
		own, folded := splitBody(m.Body)
		entries = append(entries, Entry{Tag: m.TagName, Date: m.When(), Body: own})
		entries = append(entries, folded...)
	}
	seen := map[string]bool{g.Survivor.TagName: true}
	out := entries[:0]
	for _, e := range entries {
		if !seen[e.Tag] {
			seen[e.Tag] = true
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out
}

// MergeNotes builds the survivor's new body: its own notes first, then a
// summary of everything it absorbed, then each older release's full notes.
func MergeNotes(g Group) string {
	own, _ := splitBody(g.Survivor.Body)
	entries := FoldedEntries(g)
	if len(entries) == 0 {
		return g.Survivor.Body
	}
	for drop := 0; ; drop++ {
		body := renderNotes(own, entries, drop)
		if len(body) <= maxBody || drop >= len(entries) {
			return body
		}
	}
}

func renderNotes(own string, entries []Entry, hideOldest int) string {
	var b strings.Builder
	if own != "" {
		b.WriteString(own)
		b.WriteString("\n\n")
	}
	b.WriteString(markStart + "\n\n---\n\n")
	oldest, newest := entries[len(entries)-1], entries[0]
	rng := "`" + newest.Tag + "`"
	if len(entries) > 1 {
		rng = "`" + oldest.Tag + "` → `" + newest.Tag + "`"
	}
	fmt.Fprintf(&b, "### 📦 Also includes %s\n\n", rng)
	fmt.Fprintf(&b, "This release carries the notes of %d earlier %s, compacted by [gh-compact](https://github.com/Kumarsujay03/gh-compact).\n\n",
		len(entries), plural(len(entries), "release", "releases"))

	sections := Summarize(entries)
	for _, s := range sections {
		if len(s.Items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "**%s**\n", s.Title)
		for _, it := range s.Items {
			b.WriteString("- " + it + "\n")
		}
		b.WriteString("\n")
	}

	shown := entries[:len(entries)-hideOldest]
	for _, e := range shown {
		body := e.Body
		if body == "" {
			body = "_No release notes._"
		}
		date := ""
		if !e.Date.IsZero() {
			date = " · " + e.Date.Format("2006-01-02")
		}
		fmt.Fprintf(&b, "<details><summary><b>%s</b>%s</summary>\n", html.EscapeString(e.Tag), date)
		fmt.Fprintf(&b, "<!-- gh-compact:entry tag=\"%s\" date=\"%s\" -->\n\n%s\n\n%s\n</details>\n\n",
			html.EscapeString(e.Tag), e.Date.UTC().Format(time.RFC3339), body, markEntryEnd)
	}
	if hideOldest > 0 {
		fmt.Fprintf(&b, "_Full notes of the %d oldest releases were left out to stay under GitHub's size limit._\n\n", hideOldest)
	}
	b.WriteString(markEnd)
	return b.String()
}

type Section struct {
	Title string
	Items []string
}

var (
	bulletRe    = regexp.MustCompile(`^\s{0,3}[-*+]\s+(.*)$`)
	headingRe   = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	byUserRe    = regexp.MustCompile(`\s+by @[\w-]+(\[bot\])? in https?://\S+$`)
	convRe      = regexp.MustCompile(`^(?i)(feat|fix|perf|refactor|docs|chore|build|ci|test|style)(\([^)]*\))?!?:\s*`)
	sentenceEnd = regexp.MustCompile(`[.!?](\s|$)`)
)

// Summarize pulls the top-level bullets out of every entry, keeps the first
// sentence of each, drops repeats and sorts them into features, fixes and the rest.
func Summarize(entries []Entry) []Section {
	feats := Section{Title: "✨ New"}
	fixes := Section{Title: "🐛 Fixed"}
	other := Section{Title: "🔧 Changed"}
	seen := map[string]bool{}
	add := func(text, heading string) {
		text = byUserRe.ReplaceAllString(strings.TrimSpace(text), "")
		kind := classify(text, heading)
		text = convRe.ReplaceAllString(text, "")
		text = firstSentence(text)
		key := strings.ToLower(strings.Trim(text, " .`*_"))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		switch kind {
		case "feat":
			feats.Items = append(feats.Items, text)
		case "fix":
			fixes.Items = append(fixes.Items, text)
		default:
			other.Items = append(other.Items, text)
		}
	}
	for _, e := range entries {
		bullets, headings := topBullets(e.Body)
		if len(bullets) == 0 {
			if s := firstSentence(strings.Join(strings.Fields(stripMarkdownHeadings(e.Body)), " ")); s != "" {
				add(s, "")
			}
			continue
		}
		for i, bl := range bullets {
			add(bl, headings[i])
		}
	}
	return []Section{feats, fixes, other}
}

// topBullets returns each top-level bullet (continuation lines joined) and
// the heading it sits under.
func topBullets(body string) (bullets, headings []string) {
	heading := ""
	inCode := false
	cur := -1
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
			cur = -1
			continue
		}
		if inCode {
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			heading, cur = m[1], -1
			continue
		}
		if m := bulletRe.FindStringSubmatch(line); m != nil && indent(line) < 2 {
			bullets = append(bullets, m[1])
			headings = append(headings, heading)
			cur = len(bullets) - 1
			continue
		}
		t := strings.TrimSpace(line)
		if t == "" {
			cur = -1
			continue
		}
		if cur >= 0 && indent(line) >= 2 && !bulletRe.MatchString(line) {
			bullets[cur] += " " + t
		} else if indent(line) >= 2 {
			continue // nested bullet: the parent already summarises it
		} else {
			cur = -1
		}
	}
	return
}

func classify(text, heading string) string {
	t := strings.ToLower(text)
	h := strings.ToLower(heading)
	switch {
	case strings.HasPrefix(t, "fix"), strings.Contains(h, "fix"), strings.Contains(h, "bug"),
		strings.HasPrefix(t, "bug"), strings.HasPrefix(t, "resolve"):
		return "fix"
	case strings.HasPrefix(t, "feat"), strings.HasPrefix(t, "new"), strings.HasPrefix(t, "add"),
		strings.HasPrefix(t, "introduc"), strings.HasPrefix(t, "support"),
		strings.Contains(h, "feat"), strings.Contains(h, "new"), strings.Contains(h, "add"):
		return "feat"
	}
	return "other"
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if loc := sentenceEnd.FindStringIndex(s); loc != nil && loc[0] > 10 {
		s = s[:loc[0]+1]
	}
	const limit = 140
	if utf8.RuneCountInString(s) > limit {
		r := []rune(s)
		cut := string(r[:limit])
		if i := strings.LastIndex(cut, " "); i > limit/2 {
			cut = cut[:i]
		}
		s = strings.TrimRight(cut, " ,;:(") + "…"
	}
	return s
}

func stripMarkdownHeadings(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !headingRe.MatchString(l) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func indent(line string) int {
	n := 0
	for _, c := range line {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
