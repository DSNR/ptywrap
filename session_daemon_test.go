package main

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

// Integration test: builds the binary, then drives real clients under a
// pty and raw protocol clients over the socket.

func buildBinary(t *testing.T) (bin string, env []string, runtime string) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "ptywrap")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	runtime = filepath.Join(dir, "run")
	os.Mkdir(runtime, 0o700)
	env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+runtime,
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
		"SHELL=/bin/sh",
		"PTYWRAP_SESSION=",
	)
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	return bin, env, runtime
}

// ptyClient runs `ptywrap args...` under a pty, collecting its output.
type ptyClient struct {
	cmd  *exec.Cmd
	ptmx *os.File
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
}

func startPtyClient(t *testing.T, bin string, env []string, dir string, args ...string) *ptyClient {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	c := &ptyClient{cmd: cmd, ptmx: ptmx, done: make(chan struct{})}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			c.mu.Lock()
			c.out.Write(buf[:n])
			c.mu.Unlock()
			if err != nil {
				break
			}
		}
	}()
	go func() { cmd.Wait(); close(c.done) }()
	return c
}

func (c *ptyClient) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *ptyClient) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("client did not exit; output:\n%q", c.output())
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestSessionDetachReattach(t *testing.T) {
	bin, env, runtime := buildBinary(t)
	work := t.TempDir()
	sock := filepath.Join(runtime, "ptywrap", "s1.sock")

	// 1. Attach (spawns daemon), write a marker from the shell, detach with F9.
	c1 := startPtyClient(t, bin, env, work, "attach", "s1")
	waitFor(t, "socket", func() bool { return fileExists(sock) })
	waitFor(t, "first frame", func() bool { return strings.Contains(c1.output(), "1:") })
	c1.ptmx.Write([]byte("echo before-detach > marker1\r"))
	waitFor(t, "marker1", func() bool { return fileExists(filepath.Join(work, "marker1")) })
	c1.ptmx.Write([]byte("\x1b[20~")) // F9
	c1.waitExit(t)
	if !strings.Contains(c1.output(), "[detached]") {
		t.Fatalf("expected [detached], got %q", c1.output())
	}

	// 2. Daemon survives; ls reports it.
	if !fileExists(sock) {
		t.Fatal("socket gone after detach")
	}
	out, err := exec.Command(bin, "session", "list").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "s1") {
		t.Fatalf("session list: %v\n%s", err, out)
	}
	t.Logf("session list:\n%s", out)

	// 3. Raw protocol client: reattach and check screen continuity.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(conn, msgAttach, attachHello{})
	writeJSON(conn, msgResize, winSize{100, 30})
	if f := readUntilFrame(t, conn, "before-detach"); f == "" {
		t.Fatal("reattached frame missing earlier output")
	}

	// 4. A second attach evicts the first.
	c2 := startPtyClient(t, bin, env, work, "attach", "s1")
	typ, payload, err := readMsgSkippingFrames(conn)
	if err != nil || typ != msgClose || string(payload) != reasonEvicted {
		t.Fatalf("expected eviction, got %q %q %v", typ, payload, err)
	}
	conn.Close()

	// 5. Exiting the last tab ends the session and removes the socket.
	waitFor(t, "c2 frame", func() bool { return strings.Contains(c2.output(), "before-detach") })
	c2.ptmx.Write([]byte("echo after-reattach > marker2; exit\r"))
	c2.waitExit(t)
	if !fileExists(filepath.Join(work, "marker2")) {
		t.Fatal("marker2 not written")
	}
	waitFor(t, "socket removal", func() bool { return !fileExists(sock) })
	out, _ = exec.Command(bin, "ls").CombinedOutput()
	if !strings.Contains(string(out), "no sessions") {
		t.Fatalf("expected no sessions, got %s", out)
	}
}

func readUntilFrame(t *testing.T, conn net.Conn, want string) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	for {
		typ, payload, err := readMsg(conn)
		if err != nil {
			t.Fatalf("reading frames: %v", err)
		}
		if typ == msgFrame && strings.Contains(string(payload), want) {
			return string(payload)
		}
	}
}

func readMsgSkippingFrames(conn net.Conn) (byte, []byte, error) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		typ, payload, err := readMsg(conn)
		if err != nil || typ != msgFrame {
			return typ, payload, err
		}
	}
}

func TestKeyRoundTrip(t *testing.T) {
	// tea.Key must survive the JSON wire encoding.
	var buf bytes.Buffer
	k := tea.Key{Type: tea.KeyRunes, Runes: []rune("hé"), Alt: true}
	writeJSON(&buf, msgKey, k)
	typ, payload, err := readMsg(&buf)
	if err != nil || typ != msgKey {
		t.Fatal(typ, err)
	}
	var got tea.Key
	if err := json.Unmarshal(payload, &got); err != nil || got.Type != k.Type || string(got.Runes) != "hé" || !got.Alt {
		t.Fatalf("got %+v err %v", got, err)
	}
}
