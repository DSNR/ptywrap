package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// detachKey detaches the client, leaving the session running.
const detachKey = tea.KeyF9

type frameMsg string
type disconnectMsg struct{ reason string }

// clientModel is a thin bubbletea model: input goes to the daemon, View()
// returns the last frame the daemon rendered.
type clientModel struct {
	conn     net.Conn
	frame    string
	exitText string
}

func (c *clientModel) Init() tea.Cmd { return nil }

func (c *clientModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		writeJSON(c.conn, msgResize, winSize{msg.Width, msg.Height})
	case tea.KeyMsg:
		if msg.Type == detachKey {
			writeMsg(c.conn, msgDetach, nil)
			c.exitText = "[detached]"
			return c, tea.Quit
		}
		writeJSON(c.conn, msgKey, tea.Key(msg))
	case tea.MouseMsg:
		writeJSON(c.conn, msgMouse, tea.MouseEvent(msg))
	case frameMsg:
		c.frame = string(msg)
	case disconnectMsg:
		switch msg.reason {
		case reasonEnded:
			// Last tab exited: quit silently, as a non-daemon ptywrap would.
		case reasonEvicted:
			c.exitText = "[session attached elsewhere]"
		case reasonKilled:
			c.exitText = "[session killed]"
		default:
			c.exitText = "[lost connection to session]"
		}
		return c, tea.Quit
	}
	return c, nil
}

func (c *clientModel) View() string {
	if c.frame == "" {
		return "attaching..."
	}
	return c.frame
}

// attachSession connects to the named session, starting its daemon first
// if it isn't running, and runs the interactive client until detach/exit.
func attachSession(name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	if os.Getenv(sessionEnvVar) == name {
		return fmt.Errorf("already inside session %q (detach with F9 first)", name)
	}
	if err := ensureSocketDir(); err != nil {
		return err
	}
	conn, err := dialSession(name)
	if err != nil {
		if err := spawnDaemon(name); err != nil {
			return fmt.Errorf("starting session daemon: %w", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if conn, err = dialSession(name); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("session %q did not start (see %s): %w", name, daemonLogPath(name), err)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	return runClient(conn)
}

func daemonLogPath(name string) string {
	return filepath.Join(socketDir(), name+".log")
}

// spawnDaemon re-executes this binary as a detached session daemon.
func spawnDaemon(name string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	logf, err := os.OpenFile(daemonLogPath(name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, daemonArg, name)
	cmd.Stdin = devnull
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap if the daemon exits while we're still running
	return nil
}

func runClient(conn net.Conn) error {
	defer conn.Close()
	if err := writeJSON(conn, msgAttach, attachHello{Profile: lipgloss.ColorProfile()}); err != nil {
		return err
	}
	cm := &clientModel{conn: conn}
	p := tea.NewProgram(cm, tea.WithAltScreen(), tea.WithMouseCellMotion())
	go func() {
		for {
			typ, payload, err := readMsg(conn)
			if err != nil {
				p.Send(disconnectMsg{})
				return
			}
			switch typ {
			case msgFrame:
				p.Send(frameMsg(payload))
			case msgClose:
				p.Send(disconnectMsg{string(payload)})
				return
			}
		}
	}()
	if _, err := p.Run(); err != nil {
		return err
	}
	if cm.exitText != "" {
		fmt.Println(cm.exitText)
	}
	return nil
}

// listSessions prints every live session, removing stale sockets found
// along the way.
func listSessions() error {
	paths, _ := filepath.Glob(filepath.Join(socketDir(), "*.sock"))
	var sts []sessionStatus
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".sock")
		if st, err := querySession(name); err == nil {
			sts = append(sts, st)
		}
	}
	if len(sts) == 0 {
		fmt.Println("no sessions")
		return nil
	}
	sort.Slice(sts, func(i, j int) bool { return sts[i].Name < sts[j].Name })
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTABS\tUPTIME\tATTACHED\tPID")
	for _, st := range sts {
		att := "no"
		if st.Attached {
			att = "yes"
		}
		up := time.Since(st.Started).Round(time.Second)
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d\n", st.Name, st.Tabs, up, att, st.PID)
	}
	return tw.Flush()
}

// killSession tells a running session's daemon to shut down, killing any
// shells still running in it.
func killSession(name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	conn, err := dialSession(name)
	if err != nil {
		return fmt.Errorf("session %q is not running", name)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	return writeMsg(conn, msgKill, nil)
}

// renameSession asks a running session's daemon to listen on a new name,
// atomically freeing the old name for reuse.
func renameSession(oldName, newName string) error {
	if err := validateSessionName(oldName); err != nil {
		return err
	}
	if err := validateSessionName(newName); err != nil {
		return err
	}
	conn, err := dialSession(oldName)
	if err != nil {
		return fmt.Errorf("session %q is not running", oldName)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := writeMsg(conn, msgRename, []byte(newName)); err != nil {
		return err
	}
	typ, payload, err := readMsg(conn)
	if err != nil {
		return err
	}
	if typ != msgRename {
		return fmt.Errorf("unexpected reply %q", typ)
	}
	var rep renameReply
	if err := json.Unmarshal(payload, &rep); err != nil {
		return err
	}
	if rep.Error != "" {
		return fmt.Errorf("%s", rep.Error)
	}
	return nil
}

func querySession(name string) (sessionStatus, error) {
	var st sessionStatus
	conn, err := dialSession(name)
	if err != nil {
		return st, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := writeMsg(conn, msgStatus, nil); err != nil {
		return st, err
	}
	typ, payload, err := readMsg(conn)
	if err != nil {
		return st, err
	}
	if typ != msgStatus {
		return st, fmt.Errorf("unexpected reply %q", typ)
	}
	err = json.Unmarshal(payload, &st)
	return st, err
}
