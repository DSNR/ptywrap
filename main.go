package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
	vt "ptywrap/internal/vt10x"
)

const cursorBlinkInterval = 530 * time.Millisecond
const resizeDebounce = 50 * time.Millisecond
const doubleClickWindow = 400 * time.Millisecond
const bellFlashDuration = 600 * time.Millisecond
const cursorBar = "▏"

var cursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("15"))
var cursorBarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true)

type blinkMsg struct{}

func blinkTick() tea.Cmd {
	return tea.Tick(cursorBlinkInterval, func(time.Time) tea.Msg { return blinkMsg{} })
}

type resizeApplyMsg struct{ gen int }

// Bit positions pinned to vt10x@v0.0.0-20220301184237-5011da428d02's
// unexported attr* constants (state.go); vt10x exposes no accessor for them.
const (
	attrReverse   = 1 << 0
	attrUnderline = 1 << 1
	attrBold      = 1 << 2
	attrItalic    = 1 << 4
	attrWideDummy = 1 << 7
)

func vtColor(c vt.Color, palette [16]string) lipgloss.Color {
	switch c {
	case vt.DefaultFG, vt.DefaultBG, vt.DefaultCursor:
		return lipgloss.Color("")
	}
	if c.ANSI() && palette[c] != "" {
		return lipgloss.Color(palette[c])
	}
	if c > 255 {
		return lipgloss.Color(fmt.Sprintf("#%06x", uint32(c)))
	}
	return lipgloss.Color(strconv.Itoa(int(c)))
}

func glyphStyle(g vt.Glyph, selected bool, selectionBG lipgloss.Color, palette [16]string) lipgloss.Style {
	s := lipgloss.NewStyle()
	if selected {
		return s.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("15"))
	}
	fg, bg := g.FG, g.BG
	if g.Mode&attrReverse != 0 {
		fg, bg = bg, fg
	}
	fc, bc := vtColor(fg, palette), vtColor(bg, palette)
	if fc != "" {
		s = s.Foreground(fc)
	}
	if bc != "" {
		s = s.Background(bc)
	}
	if g.Mode&attrBold != 0 {
		s = s.Bold(true)
	}
	if g.Mode&attrUnderline != 0 {
		s = s.Underline(true)
	}
	if g.Mode&attrItalic != 0 {
		s = s.Italic(true)
	}
	return s
}

type model struct {
	sessions []*session
	active   int
	width    int
	height   int
	started  bool
	blinkOn  bool
	cfg      Config
	resizeGen int
}

func (m *model) cur() *session {
	return m.sessions[m.active]
}

func (m *model) innerSize() (int, int) {
	w := m.width - 2
	h := m.height - 2 // border
	if !m.cfg.HideStatusBar {
		h-- // tab bar
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}

func (m *model) Init() tea.Cmd {
	m.blinkOn = true
	m.cfg = loadConfig()
	return blinkTick()
}

func (m *model) newTab() tea.Cmd {
	cols, rows := m.innerSize()
	idx := len(m.sessions)
	s, cmd := startSession(progHolder, idx, cols, rows, m.cfg)
	m.sessions = append(m.sessions, s)
	m.active = idx
	return cmd
}

// closeTab terminates and removes the tab at idx, adjusting the active
// index. Returns tea.Quit if it was the last tab.
func (m *model) closeTab(idx int) tea.Cmd {
	s := m.sessions[idx]
	if s.ptmx != nil {
		s.ptmx.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	m.sessions = append(m.sessions[:idx], m.sessions[idx+1:]...)
	if len(m.sessions) == 0 {
		return tea.Quit
	}
	if m.active >= len(m.sessions) {
		m.active = len(m.sessions) - 1
	}
	return nil
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.started {
			m.started = true
			return m, m.newTab()
		}
		m.resizeGen++
		gen := m.resizeGen
		return m, tea.Tick(resizeDebounce, func(time.Time) tea.Msg { return resizeApplyMsg{gen} })

	case resizeApplyMsg:
		if msg.gen != m.resizeGen {
			return m, nil
		}
		cols, rows := m.innerSize()
		for _, s := range m.sessions {
			if s.ptmx != nil {
				pty.Setsize(s.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
			}
			if s.term != nil {
				s.term.Resize(cols, rows)
			}
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case redrawMsg:
		return m, nil

	case bellMsg:
		if msg.idx < len(m.sessions) {
			m.sessions[msg.idx].bellUntil = time.Now().Add(bellFlashDuration)
		}
		return m, nil

	case blinkMsg:
		m.blinkOn = !m.blinkOn
		return m, blinkTick()

	case exitMsg:
		if msg.idx < len(m.sessions) {
			s := m.sessions[msg.idx]
			if exitedCleanly(s.cmd) {
				return m, m.closeTab(msg.idx)
			}
			s.exited = true
			s.exitErr = msg.err
		}
		return m, nil
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := m.cur()

	if s.exited {
		if msg.Type == tea.KeyEnter {
			return m, m.closeTab(m.active)
		}
		return m, nil
	}

	switch msg.Type {
	case tea.KeyF2:
		return m, m.newTab()
	case tea.KeyF3:
		return m, m.closeTab(m.active)
	case tea.KeyF4:
		m.active = (m.active - 1 + len(m.sessions)) % len(m.sessions)
		return m, nil
	case tea.KeyF6:
		m.active = (m.active + 1) % len(m.sessions)
		return m, nil
	}

	_, rows := m.innerSize()
	cols, _ := m.innerSize()

	if s.searchActive {
		switch msg.Type {
		case tea.KeyEsc:
			s.searchActive = false
		case tea.KeyEnter:
			s.searchActive = false
			s.jumpToSearch(cols, rows)
		case tea.KeyBackspace:
			if n := len(s.searchQuery); n > 0 {
				r := []rune(s.searchQuery)
				s.searchQuery = string(r[:len(r)-1])
			}
		case tea.KeyRunes, tea.KeySpace:
			s.searchQuery += msg.String()
		}
		return m, nil
	}

	switch msg.Type {
	case tea.KeyPgUp:
		s.scrollBy += rows
		s.clampScroll()
		return m, nil
	case tea.KeyPgDown:
		s.scrollBy -= rows
		s.clampScroll()
		return m, nil
	}

	if s.scrollBy > 0 {
		switch msg.String() {
		case "/":
			s.searchActive = true
			s.searchQuery = ""
			return m, nil
		case "n":
			if len(s.searchMatches) > 0 {
				s.searchNext()
				return m, nil
			}
		case "N":
			if len(s.searchMatches) > 0 {
				s.searchPrev()
				return m, nil
			}
		}
		switch msg.Type {
		case tea.KeyEnd:
			s.scrollBy = 0
			return m, nil
		case tea.KeyHome:
			s.scrollBy = s.historyLen()
			s.clampScroll()
			return m, nil
		}
	}

	s.scrollBy = 0
	m.blinkOn = true
	s.hasSelection = false
	if s.ptmx != nil {
		if msg.Paste {
			s.ptmx.Write([]byte("\x1b[200~"))
			s.ptmx.Write(keyToBytes(msg))
			s.ptmx.Write([]byte("\x1b[201~"))
		} else {
			s.ptmx.Write(keyToBytes(msg))
		}
	}
	return m, nil
}

func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	s := m.cur()
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		s.scrollBy += 3
		s.clampScroll()
		return m, nil
	case tea.MouseButtonWheelDown:
		s.scrollBy -= 3
		s.clampScroll()
		return m, nil
	}

	cols, rows := m.innerSize()
	topOffset := 1
	if !m.cfg.HideStatusBar {
		topOffset = 2 // tab bar row + border
	}
	vx := clampInt(msg.X-1, 0, cols-1)
	vy := clampInt(msg.Y-topOffset, 0, rows-1)

	// Right border column doubles as a scrollbar: click or drag there to
	// scroll through scrollback proportionally.
	if msg.X == cols+1 && s.historyLen() > 0 {
		switch msg.Action {
		case tea.MouseActionPress:
			s.draggingScrollbar = true
			s.scrollToScrollbarY(msg.Y-topOffset, rows)
			return m, nil
		case tea.MouseActionMotion:
			if s.draggingScrollbar {
				s.scrollToScrollbarY(msg.Y-topOffset, rows)
				return m, nil
			}
		case tea.MouseActionRelease:
			s.draggingScrollbar = false
			return m, nil
		}
	}
	if s.draggingScrollbar {
		if msg.Action == tea.MouseActionMotion {
			s.scrollToScrollbarY(msg.Y-topOffset, rows)
			return m, nil
		}
		s.draggingScrollbar = false
	}

	switch msg.Button {
	case tea.MouseButtonLeft:
		switch msg.Action {
		case tea.MouseActionPress:
			now := time.Now()
			if now.Sub(s.lastClickAt) < doubleClickWindow && s.lastClickPos == (cellPos{vx, vy}) {
				s.clickCount++
				if s.clickCount > 3 {
					s.clickCount = 1
				}
			} else {
				s.clickCount = 1
			}
			s.lastClickAt = now
			s.lastClickPos = cellPos{vx, vy}

			row := s.rowRunes(vy, rows, cols)
			switch s.clickCount {
			case 2:
				from, to := wordBounds(row, vx)
				s.hasSelection = true
				s.selecting = false
				s.selStart = cellPos{from, vy}
				s.selEnd = cellPos{to, vy}
				clipboard.WriteAll(s.selectionText(cols, rows))
				return m, nil
			case 3:
				s.hasSelection = true
				s.selecting = false
				s.selStart = cellPos{0, vy}
				s.selEnd = cellPos{cols - 1, vy}
				clipboard.WriteAll(s.selectionText(cols, rows))
				return m, nil
			}

			if msg.Ctrl {
				if u := urlAtCol(row, vx); u != "" {
					openURL(u)
					return m, nil
				}
			}
			s.selecting = true
			s.hasSelection = false
			s.selStart = cellPos{vx, vy}
			s.selEnd = cellPos{vx, vy}
		case tea.MouseActionMotion:
			if s.selecting {
				s.selEnd = cellPos{vx, vy}
			}
		case tea.MouseActionRelease:
			if s.selecting {
				s.selEnd = cellPos{vx, vy}
				s.selecting = false
				s.hasSelection = s.selStart != s.selEnd
				if s.hasSelection {
					clipboard.WriteAll(s.selectionText(cols, rows))
				}
			}
		}
	case tea.MouseButtonRight:
		if msg.Action == tea.MouseActionPress {
			s.hasSelection = false
			if text, err := clipboard.ReadAll(); err == nil && s.ptmx != nil {
				s.ptmx.Write([]byte("\x1b[200~"))
				s.ptmx.Write([]byte(text))
				s.ptmx.Write([]byte("\x1b[201~"))
			}
		}
	}
	return m, nil
}

// wordBounds returns the [from,to] rune index range (inclusive) of the
// whitespace-delimited word containing col in row.
func wordBounds(row []rune, col int) (int, int) {
	if col < 0 || col >= len(row) {
		return col, col
	}
	isSpace := func(r rune) bool { return r == ' ' || r == 0 }
	from, to := col, col
	for from > 0 && !isSpace(row[from-1]) {
		from--
	}
	for to < len(row)-1 && !isSpace(row[to+1]) {
		to++
	}
	return from, to
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func overlayTag(borderLine, tag string) string {
	r := []rune(borderLine)
	t := []rune(tag)
	if len(r) < len(t)+4 {
		return borderLine
	}
	copy(r[2:2+len(t)], t)
	return string(r)
}

// overlayTagCenter writes tag centered into borderLine, leaving the corner
// runes intact.
func overlayTagCenter(borderLine, tag string) string {
	r := []rune(borderLine)
	t := []rune(tag)
	if len(r) < len(t)+4 {
		return borderLine
	}
	start := (len(r) - len(t)) / 2
	copy(r[start:start+len(t)], t)
	return string(r)
}

func (m *model) renderTabBar() string {
	plain := lipgloss.NewStyle().Padding(0, 1)
	active := plain.Bold(true).Reverse(true)
	var parts []string
	for i, s := range m.sessions {
		label := fmt.Sprintf("%d:%s", i+1, s.tabLabel())
		if s.exited {
			label += " [x]"
		}
		if i == m.active {
			parts = append(parts, active.Render(label))
		} else {
			parts = append(parts, plain.Render(label))
		}
	}
	return strings.Join(parts, "")
}

func (m *model) View() string {
	if len(m.sessions) == 0 {
		return "starting..."
	}
	s := m.cur()
	if s.term == nil {
		return "starting..."
	}
	cols, rows := m.innerSize()
	s.term.Lock()
	defer s.term.Unlock()
	// The pty's goroutine may still be resizing (debounced) when a
	// WindowSizeMsg lands, so clamp against the term's actual current size
	// to avoid reading out-of-bounds cells mid-resize.
	if tc, tr := s.term.Size(); tc > 0 && tr > 0 {
		if cols > tc {
			cols = tc
		}
		if rows > tr {
			rows = tr
		}
	}
	cur := s.term.Cursor()
	showCursor := s.scrollBy == 0 && s.term.CursorVisible() && m.blinkOn

	theme := m.cfg.Theme
	selectionBG := lipgloss.Color(theme.SelectionColor)
	if theme.SelectionColor == "" {
		selectionBG = lipgloss.Color("237")
	}
	searchStyle := lipgloss.NewStyle().Background(lipgloss.Color(orDefault(theme.SearchColor, "58")))

	var matchLine = -1
	if len(s.searchMatches) > 0 && s.searchIdx < len(s.searchMatches) {
		h := s.historyLen()
		matchLine = s.searchMatches[s.searchIdx] - (h - s.scrollBy)
	}

	var b strings.Builder
	for y := 0; y < rows; y++ {
		var run strings.Builder
		runFG, runBG, runMode := vt.Color(vt.DefaultFG), vt.Color(vt.DefaultBG), int16(0)
		runSelected := false
		flush := func() {
			if run.Len() == 0 {
				return
			}
			style := glyphStyle(vt.Glyph{FG: runFG, BG: runBG, Mode: runMode}, runSelected, selectionBG, theme.Palette)
			if y == matchLine {
				style = searchStyle
			}
			b.WriteString(style.Render(run.String()))
			run.Reset()
		}
		for x := 0; x < cols; x++ {
			g := s.cellAt(x, y, rows)
			if g.Mode&attrWideDummy != 0 {
				continue
			}
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			if showCursor && x == cur.X && y == cur.Y {
				flush()
				if m.cfg.CursorStyle == "bar" {
					b.WriteString(cursorBarStyle.Render(cursorBar))
				} else {
					b.WriteString(cursorStyle.Render(string(ch)))
				}
				continue
			}
			selected := s.inSelection(x, y)
			if g.FG != runFG || g.BG != runBG || g.Mode != runMode || selected != runSelected {
				flush()
				runFG, runBG, runMode = g.FG, g.BG, g.Mode
				runSelected = selected
			}
			run.WriteRune(ch)
		}
		flush()
		if y < rows-1 {
			b.WriteByte('\n')
		}
	}

	borderColor := theme.BorderColor
	if target := s.sshTarget(); target != "" {
		borderColor = theme.sshColorFor(target)
	}
	if time.Now().Before(s.bellUntil) {
		borderColor = orDefault(theme.BellColor, "196")
	}
	borderRune := lipgloss.NewStyle()
	if borderColor != "" {
		borderRune = borderRune.Foreground(lipgloss.Color(borderColor))
	}

	// Build the box by hand rather than via lipgloss's Border(): the
	// content lines already carry per-cell ANSI color codes, so splicing
	// tag/scrollbar text into an already-rendered bordered string (as
	// overlayTag does) lands on escape-sequence bytes, not glyphs. Instead
	// we edit plain top/bottom border text first, then color it once.
	bd := lipgloss.RoundedBorder()
	topPlain := bd.TopLeft + strings.Repeat(bd.Top, cols) + bd.TopRight
	bottomPlain := bd.BottomLeft + strings.Repeat(bd.Bottom, cols) + bd.BottomRight

	if s.scrollBy > 0 {
		tag := fmt.Sprintf(" SCROLL -%d ", s.scrollBy)
		if len(s.searchMatches) > 0 {
			tag = fmt.Sprintf(" SCROLL -%d  match %d/%d ", s.scrollBy, s.searchIdx+1, len(s.searchMatches))
		}
		topPlain = overlayTag(topPlain, tag)
	}

	tag := s.cwd()
	if target := s.sshTarget(); target != "" {
		tag = "ssh:" + target
	}
	if tag != "" {
		bottomPlain = overlayTagCenter(bottomPlain, " "+tag+" ")
	}

	contentLines := strings.Split(b.String(), "\n")
	thumbFrom, thumbTo, showThumb := s.scrollbarThumb(rows)
	lines := make([]string, 0, rows+2)
	lines = append(lines, borderRune.Render(topPlain))
	for y := 0; y < rows; y++ {
		right := bd.Right
		if showThumb && y >= thumbFrom && y <= thumbTo {
			right = "█"
		}
		lines = append(lines, borderRune.Render(bd.Left)+contentLines[y]+borderRune.Render(right))
	}
	lines = append(lines, borderRune.Render(bottomPlain))
	out := strings.Join(lines, "\n")

	var statusLine string
	switch {
	case s.searchActive:
		statusLine = "/" + s.searchQuery
	case s.exited:
		statusLine = fmt.Sprintf("[%s — press enter to close tab]", exitStatus(s.cmd, s.exitErr))
	}

	result := out
	if !m.cfg.HideStatusBar {
		result = m.renderTabBar() + "\n" + out
	}
	if statusLine != "" {
		result += "\n" + statusLine
	}
	return result
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func printUsage() {
	fmt.Println(`ptywrap - terminal multiplexer TUI

Usage:
  ptywrap [options]

Options:
  --init-config   Write a default config file (without overwriting an existing one)
  -h, --help      Show this help message`)
}

var progHolder *tea.Program

func main() {
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-h", "-help", "--help":
			printUsage()
			return
		case "-init-config", "--init-config":
			if err := writeDefaultConfig(); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			fmt.Println("wrote", configPath())
			return
		}
	}

	m := &model{}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	progHolder = p
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
