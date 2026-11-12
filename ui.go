package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/cli/go-gh/v2/pkg/term"
)

var (
	cPink   = lipgloss.AdaptiveColor{Light: "#C2185B", Dark: "#FF6AC1"}
	cViolet = lipgloss.AdaptiveColor{Light: "#5E35B1", Dark: "#B39DFF"}
	cCyan   = lipgloss.AdaptiveColor{Light: "#00838F", Dark: "#5CE1E6"}
	cGreen  = lipgloss.AdaptiveColor{Light: "#2E7D32", Dark: "#7CF29A"}
	cAmber  = lipgloss.AdaptiveColor{Light: "#B26A00", Dark: "#FFD166"}
	cRed    = lipgloss.AdaptiveColor{Light: "#C62828", Dark: "#FF6B6B"}
	cDim    = lipgloss.AdaptiveColor{Light: "#757575", Dark: "#8A8F98"}

	sBrand   = lipgloss.NewStyle().Bold(true).Foreground(cPink)
	sRepo    = lipgloss.NewStyle().Bold(true).Foreground(cViolet)
	sKeep    = lipgloss.NewStyle().Bold(true).Foreground(cGreen)
	sDrop    = lipgloss.NewStyle().Foreground(cRed).Strikethrough(true)
	sTag     = lipgloss.NewStyle().Foreground(cCyan)
	sWarn    = lipgloss.NewStyle().Foreground(cAmber)
	sErr     = lipgloss.NewStyle().Bold(true).Foreground(cRed)
	sOK      = lipgloss.NewStyle().Bold(true).Foreground(cGreen)
	sDim     = lipgloss.NewStyle().Foreground(cDim)
	sBold    = lipgloss.NewStyle().Bold(true)
	sPrivate = lipgloss.NewStyle().Foreground(cAmber).Faint(true)
	sBox     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cViolet).Padding(0, 2)
)

var termIO = term.FromEnv()

func isTTY() bool { return termIO.IsTerminalOutput() }

// interactive is true when someone can answer a prompt.
func interactive() bool { return term.IsTerminal(os.Stdin) && term.IsTerminal(os.Stderr) }

func banner(w io.Writer) { bannerFor(w, "fold noisy releases into tidy ones") }

func bannerFor(w io.Writer, tagline string) {
	fmt.Fprintln(w, sBrand.Render("◆ gh compact")+sDim.Render("  "+tagline))
}

// spinner draws on stderr while slow work runs; silent when not a terminal.
type spinner struct {
	mu   sync.Mutex
	msg  string
	stop chan struct{}
	done chan struct{}
}

func startSpinner(msg string) *spinner {
	s := &spinner{msg: msg, stop: make(chan struct{}), done: make(chan struct{})}
	if !term.IsTerminal(os.Stderr) {
		close(s.done)
		return s
	}
	go func() {
		defer close(s.done)
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		for i := 0; ; i++ {
			s.mu.Lock()
			fmt.Fprintf(os.Stderr, "\r\033[K%s %s", sBrand.Render(frames[i%len(frames)]), s.msg)
			s.mu.Unlock()
			select {
			case <-s.stop:
				fmt.Fprint(os.Stderr, "\r\033[K")
				return
			case <-time.After(80 * time.Millisecond):
			}
		}
	}()
	return s
}

func (s *spinner) Set(msg string) { s.mu.Lock(); s.msg = msg; s.mu.Unlock() }

func (s *spinner) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

// RepoPlan is what a scan found for one repository.
type RepoPlan struct {
	Repo     Repo
	Releases int
	Groups   []Group
	Err      error
}

func (p RepoPlan) Removed() (n int) {
	for _, g := range p.Groups {
		n += len(g.Members)
	}
	return
}

func renderPlan(w io.Writer, p RepoPlan) {
	vis := ""
	if p.Repo.Private {
		vis = " " + sPrivate.Render("private")
	}
	ro := ""
	if !p.Repo.CanWrite() {
		ro = " " + sWarn.Render("read-only · plan only")
	}
	fmt.Fprintf(w, "\n%s %s%s%s  %s\n", sBrand.Render("◆"), sRepo.Render(p.Repo.FullName), vis, ro,
		sDim.Render(fmt.Sprintf("%d releases → %d", p.Releases, p.Releases-p.Removed())))
	for i, g := range p.Groups {
		branch := "├─"
		if i == len(p.Groups)-1 {
			branch = "└─"
		}
		var drops []string
		for _, m := range g.Members {
			drops = append(drops, sDrop.Render(m.TagName))
		}
		info := []string{"spans " + human(g.Span())}
		if n, b := g.DroppedAssets(); n > 0 {
			info = append(info, sWarn.Render(fmt.Sprintf("drops %d old %s (%s)", n, plural(n, "asset", "assets"), bytesHuman(b))))
		}
		fmt.Fprintf(w, "  %s %s %s %s  %s\n", sDim.Render(branch), sKeep.Render(g.Survivor.TagName),
			sDim.Render("⟵"), strings.Join(drops, " "), sDim.Render(strings.Join(info, " · ")))
	}
}

func human(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func bytesHuman(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

var stdin = bufio.NewReader(os.Stdin)

func ask(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	line, _ := stdin.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line))
}
