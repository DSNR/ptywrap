package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

const frameInterval = 16 * time.Millisecond
const frameWriteTimeout = 5 * time.Second

// startTimeout bounds how long a daemon waits for its first client to
// report a window size (which creates the first tab) before giving up.
const startTimeout = 30 * time.Second

// chanSender adapts the daemon's event channel to msgSender, standing in
// for *tea.Program.
type chanSender chan tea.Msg

func (c chanSender) Send(msg tea.Msg) { c <- msg }

// clientConn is the daemon's side of one attached client.
type clientConn struct {
	conn      net.Conn
	mu        sync.Mutex  // serializes writes to conn
	frames    chan []byte // cap 1: only the latest frame is kept
	done      chan struct{}
	closeOnce sync.Once
}

func newClientConn(conn net.Conn) *clientConn {
	c := &clientConn{conn: conn, frames: make(chan []byte, 1), done: make(chan struct{})}
	go c.writeLoop()
	return c
}

// pushFrame replaces any not-yet-sent frame with f. Only called from the
// daemon's event loop, so drain-then-push can't race another producer.
func (c *clientConn) pushFrame(f []byte) {
	select {
	case <-c.frames:
	default:
	}
	select {
	case c.frames <- f:
	default:
	}
}

func (c *clientConn) writeLoop() {
	for {
		select {
		case f := <-c.frames:
			c.mu.Lock()
			c.conn.SetWriteDeadline(time.Now().Add(frameWriteTimeout))
			err := writeMsg(c.conn, msgFrame, f)
			c.mu.Unlock()
			if err != nil {
				c.close("")
				return
			}
		case <-c.done:
			return
		}
	}
}

// close tells the client why (if reason != "") and drops the connection.
func (c *clientConn) close(reason string) {
	c.closeOnce.Do(func() {
		close(c.done)
		if reason != "" {
			c.mu.Lock()
			c.conn.SetWriteDeadline(time.Now().Add(time.Second))
			writeMsg(c.conn, msgClose, []byte(reason))
			c.mu.Unlock()
		}
		c.conn.Close()
	})
}

// Event-loop messages originating from client connections.
type clientAttachMsg struct {
	c       *clientConn
	profile termenv.Profile
}
type clientInputMsg struct {
	c   *clientConn
	msg tea.Msg
}
type clientGoneMsg struct{ c *clientConn }
type statusReqMsg struct{ reply chan sessionStatus }

type daemon struct {
	name    string
	ln      net.Listener
	ch      chan tea.Msg
	m       *model
	client  *clientConn
	started time.Time
}

// runDaemon serves the named session on its unix socket. It replaces
// bubbletea's runtime with a small event loop: messages from the attached
// client and from the session goroutines are fed through model.Update,
// and model.View() frames are pushed back to the client.
func runDaemon(name string) error {
	if err := validateSessionName(name); err != nil {
		return err
	}
	if err := ensureSocketDir(); err != nil {
		return err
	}
	path := socketPath(name)
	if conn, err := dialSession(name); err == nil {
		conn.Close()
		return fmt.Errorf("session %q is already running", name)
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer ln.Close() // unlinks the socket

	os.Setenv(sessionEnvVar, name)
	// No TTY here to detect; render truecolor until a client reports its
	// own profile on attach.
	lipgloss.SetColorProfile(termenv.TrueColor)

	ch := make(chan tea.Msg, 256)
	progHolder = chanSender(ch)
	d := &daemon{name: name, ln: ln, ch: ch, m: &model{}, started: time.Now()}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	go d.acceptLoop()
	d.loop(sig)
	return nil
}

func (d *daemon) acceptLoop() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		go d.handleConn(conn)
	}
}

func (d *daemon) handleConn(conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	typ, payload, err := readMsg(conn)
	if err != nil {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})

	switch typ {
	case msgStatus:
		defer conn.Close()
		reply := make(chan sessionStatus, 1)
		d.ch <- statusReqMsg{reply}
		select {
		case st := <-reply:
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			writeJSON(conn, msgStatus, st)
		case <-time.After(2 * time.Second):
		}
		return
	case msgAttach:
	default:
		conn.Close()
		return
	}

	var hello attachHello
	json.Unmarshal(payload, &hello)
	c := newClientConn(conn)
	d.ch <- clientAttachMsg{c, hello.Profile}

	for {
		typ, payload, err := readMsg(conn)
		if err != nil {
			d.ch <- clientGoneMsg{c}
			return
		}
		var msg tea.Msg
		switch typ {
		case msgResize:
			var ws winSize
			if json.Unmarshal(payload, &ws) == nil {
				msg = tea.WindowSizeMsg{Width: ws.Width, Height: ws.Height}
			}
		case msgKey:
			var k tea.Key
			if json.Unmarshal(payload, &k) == nil {
				msg = tea.KeyMsg(k)
			}
		case msgMouse:
			var me tea.MouseEvent
			if json.Unmarshal(payload, &me) == nil {
				msg = tea.MouseMsg(me)
			}
		case msgDetach:
			d.ch <- clientGoneMsg{c}
			return
		}
		if msg != nil {
			d.ch <- clientInputMsg{c, msg}
		}
	}
}

func (d *daemon) loop(sig chan os.Signal) {
	d.run(d.m.Init())
	startDeadline := time.After(startTimeout)
	var renderC <-chan time.Time
	var lastFrame string
	markDirty := func() {
		if renderC == nil && d.client != nil {
			renderC = time.After(frameInterval)
		}
	}

	for {
		select {
		case <-sig:
			d.shutdown(reasonKilled)
			return

		case <-startDeadline:
			if !d.m.started {
				d.shutdown(reasonEnded)
				return
			}

		case <-renderC:
			renderC = nil
			if d.client != nil {
				if f := d.m.View(); f != lastFrame {
					d.client.pushFrame([]byte(f))
					lastFrame = f
				}
			}

		case msg := <-d.ch:
			switch msg := msg.(type) {
			case tea.QuitMsg:
				d.shutdown(reasonEnded)
				return
			case tea.BatchMsg:
				for _, cmd := range msg {
					d.run(cmd)
				}
			case clientAttachMsg:
				if d.client != nil {
					d.client.close(reasonEvicted)
				}
				d.client = msg.c
				lipgloss.SetColorProfile(msg.profile)
				lastFrame = ""
				markDirty()
			case clientGoneMsg:
				if d.client == msg.c {
					d.client = nil
				}
				msg.c.close("")
			case clientInputMsg:
				if msg.c == d.client {
					d.update(msg.msg)
					markDirty()
				}
			case statusReqMsg:
				msg.reply <- sessionStatus{
					Name:     d.name,
					PID:      os.Getpid(),
					Tabs:     len(d.m.sessions),
					Started:  d.started,
					Attached: d.client != nil,
				}
			default:
				d.update(msg)
				markDirty()
			}
			if d.m.started && len(d.m.sessions) == 0 {
				d.shutdown(reasonEnded)
				return
			}
		}
	}
}

func (d *daemon) update(msg tea.Msg) {
	_, cmd := d.m.Update(msg)
	d.run(cmd)
}

// run executes a tea.Cmd off the event loop and feeds its result back in.
func (d *daemon) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		if msg := cmd(); msg != nil {
			d.ch <- msg
		}
	}()
}

// shutdown removes the socket first (so a fresh attach starts a new
// daemon rather than reaching this dying one), then notifies the client
// and kills any remaining shells.
func (d *daemon) shutdown(reason string) {
	d.ln.Close()
	if d.client != nil {
		d.client.close(reason)
		d.client = nil
	}
	for len(d.m.sessions) > 0 {
		d.m.closeTab(0)
	}
}
