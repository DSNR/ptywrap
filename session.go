package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	vt "ptywrap/internal/vt10x"
)

// scrollbackReader is satisfied by the vendored vt10x terminal's embedded
// *State; vt.Terminal itself exposes no scrollback accessors.
type scrollbackReader interface {
	HistoryLen() int
	HistoryCell(x, n int) vt.Glyph
}

// bellReader is satisfied by vt10x's *State; exposes the BEL counter.
type bellReader interface {
	ConsumeBell() int
}

type cellPos struct{ X, Y int }

// session is a single shell/pty pane shown as one tab.
type session struct {
	term vt.Terminal
	ptmx *os.File
	cmd  *exec.Cmd
	log  *os.File

	scrollBy int // lines scrolled back from live tail; 0 = live

	selecting    bool
	hasSelection bool
	selStart     cellPos
	selEnd       cellPos

	draggingScrollbar bool

	lastClickAt  time.Time
	lastClickPos cellPos
	clickCount   int

	searchActive bool // editing a query right now
	searchQuery  string
	searchMatches []int // absolute line indices (0 = oldest history line)
	searchIdx     int

	bellUntil time.Time

	exited  bool
	exitErr error
}

// normSelection returns the selection as (fromY,fromX)-(toY,toX), ordered.
func (s *session) normSelection() (cellPos, cellPos) {
	a, b := s.selStart, s.selEnd
	if a.Y > b.Y || (a.Y == b.Y && a.X > b.X) {
		a, b = b, a
	}
	return a, b
}

func (s *session) inSelection(x, y int) bool {
	if !s.hasSelection && !s.selecting {
		return false
	}
	from, to := s.normSelection()
	if y < from.Y || y > to.Y {
		return false
	}
	if y == from.Y && x < from.X {
		return false
	}
	if y == to.Y && x > to.X {
		return false
	}
	return true
}

func (s *session) historyLen() int {
	sb, ok := s.term.(scrollbackReader)
	if !ok {
		return 0
	}
	return sb.HistoryLen()
}

func (s *session) clampScroll() {
	if s.scrollBy < 0 {
		s.scrollBy = 0
	}
	if max := s.historyLen(); s.scrollBy > max {
		s.scrollBy = max
	}
}

// cellAt returns the glyph for viewport row y (0 at top of the visible
// pane), honoring the current scrollback offset.
func (s *session) cellAt(x, y, rows int) vt.Glyph {
	h := s.historyLen()
	start := h - s.scrollBy
	line := start + y
	if line < h {
		sb := s.term.(scrollbackReader)
		return sb.HistoryCell(x, line)
	}
	return s.term.Cell(x, line-h)
}

// absLineText returns the plain text (wide-char filler cells skipped) of
// absolute line n, where 0 is the oldest scrollback line and lines >=
// historyLen() are the live screen rows.
func (s *session) absLineText(n, cols int) string {
	h := s.historyLen()
	var g func(x int) vt.Glyph
	if n < h {
		sb := s.term.(scrollbackReader)
		g = func(x int) vt.Glyph { return sb.HistoryCell(x, n) }
	} else {
		g = func(x int) vt.Glyph { return s.term.Cell(x, n-h) }
	}
	var b strings.Builder
	for x := 0; x < cols; x++ {
		c := g(x)
		if c.Mode&attrWideDummy != 0 {
			continue
		}
		if c.Char == 0 {
			b.WriteRune(' ')
		} else {
			b.WriteRune(c.Char)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

func (s *session) rowRunes(y, rows, cols int) []rune {
	line := make([]rune, 0, cols)
	for x := 0; x < cols; x++ {
		g := s.cellAt(x, y, rows)
		if g.Mode&attrWideDummy != 0 {
			continue
		}
		if g.Char == 0 {
			line = append(line, ' ')
		} else {
			line = append(line, g.Char)
		}
	}
	return line
}

func (s *session) selectionText(cols, rows int) string {
	from, to := s.normSelection()
	var lines []string
	for y := from.Y; y <= to.Y; y++ {
		xFrom, xTo := 0, cols-1
		if y == from.Y {
			xFrom = from.X
		}
		if y == to.Y {
			xTo = to.X
		}
		var line strings.Builder
		for x := xFrom; x <= xTo; x++ {
			g := s.cellAt(x, y, rows)
			if g.Mode&attrWideDummy != 0 {
				continue
			}
			if g.Char == 0 {
				line.WriteRune(' ')
			} else {
				line.WriteRune(g.Char)
			}
		}
		lines = append(lines, strings.TrimRight(line.String(), " "))
	}
	return strings.Join(lines, "\n")
}

// cwd returns the shell process's current working directory by reading its
// /proc/<pid>/cwd symlink. Empty if unavailable.
func (s *session) cwd() string {
	if s.cmd == nil || s.cmd.Process == nil {
		return ""
	}
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", s.cmd.Process.Pid))
	if err != nil {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if link == home {
			return "~"
		}
		if strings.HasPrefix(link, home+"/") {
			return "~" + link[len(home):]
		}
	}
	return link
}

// sshForegroundPID returns the pty's foreground process group id and
// whether its command is ssh.
func (s *session) sshForegroundPID() (int, bool) {
	if s.ptmx == nil {
		return 0, false
	}
	pgid, err := unix.IoctlGetInt(int(s.ptmx.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return 0, false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pgid))
	if err != nil {
		return 0, false
	}
	return pgid, strings.TrimSpace(string(comm)) == "ssh"
}

// sshActive reports whether the pty's current foreground process (the job
// the user is actively interacting with) is ssh.
func (s *session) sshActive() bool {
	_, ok := s.sshForegroundPID()
	return ok
}

// sshTarget returns the destination (user@host) argument of the active
// foreground ssh process, parsed from its /proc cmdline. Empty if unavailable.
func (s *session) sshTarget() string {
	pgid, ok := s.sshForegroundPID()
	if !ok {
		return ""
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pgid))
	if err != nil {
		return ""
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(args) < 2 {
		return ""
	}
	// ssh [options] destination [command]; options taking a value consume
	// the following argument.
	valueFlags := "bcDeEFiJLlmOopQRSWw"
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if len(a) >= 2 && a[0] == '-' {
			if strings.ContainsRune(valueFlags, rune(a[1])) && len(a) == 2 {
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// tabLabel returns a short name for the tab bar: the terminal title if set,
// else the shell command's base name.
func (s *session) tabLabel() string {
	if t := s.term.Title(); t != "" {
		return t
	}
	if s.cmd != nil && len(s.cmd.Args) > 0 {
		return filepath.Base(s.cmd.Args[0])
	}
	return "shell"
}

// runSearch scans all lines (scrollback + screen) for query and records
// absolute line indices of matches, oldest first.
func (s *session) runSearch(cols, rows int) {
	s.searchMatches = nil
	if s.searchQuery == "" {
		return
	}
	total := s.historyLen() + rows
	for n := 0; n < total; n++ {
		if strings.Contains(s.absLineText(n, cols), s.searchQuery) {
			s.searchMatches = append(s.searchMatches, n)
		}
	}
}

// scrollToScrollbarY sets scrollBy from a click/drag at viewport row y (0 at
// top of the pane) on the scrollbar track, mapping top of track to the
// oldest scrollback line and bottom to live.
func (s *session) scrollToScrollbarY(y, rows int) {
	h := s.historyLen()
	if rows <= 1 {
		s.scrollBy = h
	} else {
		frac := 1 - float64(clampInt(y, 0, rows-1))/float64(rows-1)
		s.scrollBy = int(frac*float64(h) + 0.5)
	}
	s.clampScroll()
}

// scrollbarThumb returns the [from,to] viewport rows (inclusive) the
// scrollbar thumb should occupy for the current scroll position, and
// whether a scrollbar should be shown at all.
func (s *session) scrollbarThumb(rows int) (int, int, bool) {
	h := s.historyLen()
	if h == 0 {
		return 0, 0, false
	}
	total := h + rows
	thumbLen := clampInt(rows*rows/total, 1, rows)
	top := h - s.scrollBy
	thumbStart := clampInt(top*rows/total, 0, rows-thumbLen)
	return thumbStart, thumbStart + thumbLen - 1, true
}

// scrollToAbsLine sets scrollBy so absolute line n is the top visible row.
func (s *session) scrollToAbsLine(n int) {
	h := s.historyLen()
	s.scrollBy = h - n
	s.clampScroll()
}

// jumpToMatch searches for the match closest to (and at or before) the
// current view, landing on the first search.
func (s *session) jumpToSearch(cols, rows int) {
	s.runSearch(cols, rows)
	if len(s.searchMatches) == 0 {
		return
	}
	h := s.historyLen()
	cur := h - s.scrollBy
	// pick the newest match at or before the current top line, else the
	// newest match overall.
	best := 0
	for i, n := range s.searchMatches {
		if n <= cur {
			best = i
		}
	}
	s.searchIdx = best
	s.scrollToAbsLine(s.searchMatches[s.searchIdx])
}

func (s *session) searchNext() {
	if len(s.searchMatches) == 0 {
		return
	}
	s.searchIdx--
	if s.searchIdx < 0 {
		s.searchIdx = len(s.searchMatches) - 1
	}
	s.scrollToAbsLine(s.searchMatches[s.searchIdx])
}

func (s *session) searchPrev() {
	if len(s.searchMatches) == 0 {
		return
	}
	s.searchIdx++
	if s.searchIdx >= len(s.searchMatches) {
		s.searchIdx = 0
	}
	s.scrollToAbsLine(s.searchMatches[s.searchIdx])
}

type redrawMsg struct{ idx int }
type exitMsg struct {
	idx int
	err error
}
type bellMsg struct{ idx int }

// exitedCleanly reports whether cmd finished with status 0; used to decide
// whether a tab can close silently or needs to show its exit status.
func exitedCleanly(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0
}

// exitStatus renders a short human-readable summary of how the shell exited.
// cmd's own exit code, when available, takes precedence: a Linux pty's
// master side reads back EIO once the last slave fd closes, which is just
// the normal end-of-session signal and not a real I/O failure.
func exitStatus(cmd *exec.Cmd, err error) string {
	if cmd != nil && cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code > 0 {
			return fmt.Sprintf("exited: code %d", code)
		}
		return "exited"
	}
	if err == nil || err == io.EOF || errors.Is(err, syscall.EIO) {
		return "exited"
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Sprintf("exited: code %d", ee.ExitCode())
	}
	return fmt.Sprintf("exited: %v", err)
}

// startSession spawns a shell in a new pty and returns the session plus a
// tea.Cmd that launches its reader goroutine.
func startSession(p *tea.Program, idx int, cols, rows int, cfg Config) (*session, tea.Cmd) {
	s := &session{}
	cmd1 := func() tea.Msg {
		shell := cfg.Shell
		if shell == "" {
			shell = os.Getenv("SHELL")
		}
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd := exec.Command(shell)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
		if err != nil {
			return exitMsg{idx, err}
		}
		s.ptmx = ptmx
		s.cmd = cmd
		s.term = vt.New(vt.WithSize(cols, rows))
		if sb, ok := s.term.(interface{ SetHistoryLimit(int) }); ok {
			sb.SetHistoryLimit(cfg.ScrollbackSize)
		}

		if cfg.LogDir != "" {
			if err := os.MkdirAll(cfg.LogDir, 0o755); err == nil {
				name := fmt.Sprintf("ptywrap-%d-%d.log", os.Getpid(), idx)
				if f, err := os.Create(filepath.Join(cfg.LogDir, name)); err == nil {
					s.log = f
				}
			}
		}

		go func() {
			var r io.Reader = ptmx
			if s.log != nil {
				r = io.TeeReader(ptmx, s.log)
			}
			br := bufio.NewReader(r)
			for {
				if err := s.term.Parse(br); err != nil {
					cmd.Wait()
					if s.log != nil {
						s.log.Close()
					}
					p.Send(exitMsg{idx, err})
					return
				}
				if bl, ok := s.term.(bellReader); ok && bl.ConsumeBell() > 0 {
					p.Send(bellMsg{idx})
				}
				p.Send(redrawMsg{idx})
			}
		}()
		return redrawMsg{idx}
	}
	return s, cmd1
}
