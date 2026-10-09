package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/muesli/termenv"
)

// Wire protocol between client and daemon: every message is a 4-byte
// big-endian length N, then N bytes: a one-byte type tag + payload.
const (
	msgAttach byte = 'A' // c→d: attachHello (JSON); evicts any current client
	msgResize byte = 'R' // c→d: winSize (JSON)
	msgKey    byte = 'K' // c→d: tea.Key (JSON)
	msgMouse  byte = 'M' // c→d: tea.MouseEvent (JSON)
	msgDetach byte = 'D' // c→d: no payload
	msgStatus byte = 'S' // c→d: status request (no payload); d→c: sessionStatus (JSON)
	msgFrame  byte = 'F' // d→c: raw rendered frame
	msgClose  byte = 'X' // d→c: reason string; daemon closes the conn after
)

// Reasons sent with msgClose.
const (
	reasonEnded   = "ended"
	reasonEvicted = "attached elsewhere"
	reasonKilled  = "killed"
)

const maxMsgLen = 64 << 20

const defaultSessionName = "main"

// daemonArg is the hidden argv[1] that runs the process as a session daemon.
const daemonArg = "__daemon"

// sessionEnvVar is set in every shell a daemon spawns, to stop a client
// from attaching to the session it is running inside.
const sessionEnvVar = "PTYWRAP_SESSION"

type attachHello struct {
	Profile termenv.Profile `json:"profile"`
}

type winSize struct {
	Width  int `json:"w"`
	Height int `json:"h"`
}

type sessionStatus struct {
	Name     string    `json:"name"`
	PID      int       `json:"pid"`
	Tabs     int       `json:"tabs"`
	Started  time.Time `json:"started"`
	Attached bool      `json:"attached"`
}

func writeMsg(w io.Writer, typ byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(1+len(payload)))
	buf[4] = typ
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

func writeJSON(w io.Writer, typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeMsg(w, typ, b)
}

func readMsg(r io.Reader) (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxMsgLen {
		return 0, nil, fmt.Errorf("bad message length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return buf[0], buf[1:], nil
}

func socketDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "ptywrap")
	}
	return fmt.Sprintf("/tmp/ptywrap-%d", os.Getuid())
}

func socketPath(name string) string {
	return filepath.Join(socketDir(), name+".sock")
}

// ensureSocketDir creates the socket directory (mode 0700) and refuses to
// use one that is a symlink or owned by another user.
func ensureSocketDir() error {
	dir := socketDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || (ok && int(st.Uid) != os.Getuid()) {
		return fmt.Errorf("socket dir %s is not a directory owned by you", dir)
	}
	return nil
}

var sessionNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

func validateSessionName(name string) error {
	if !sessionNameRe.MatchString(name) {
		return fmt.Errorf("invalid session name %q (use letters, digits, '_', '-', '.')", name)
	}
	return nil
}

// dialSession connects to a session's socket. A socket file whose listener
// is gone (connection refused) is stale and gets removed.
func dialSession(name string) (net.Conn, error) {
	path := socketPath(name)
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil && errors.Is(err, syscall.ECONNREFUSED) {
		os.Remove(path)
	}
	return conn, err
}
