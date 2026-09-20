package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

const (
	// RemotePrefix is where the remote API lives on the public port. Anything
	// else is proxied to llama-server.
	RemotePrefix = "/_hfsd"
	TokenHeader  = "X-Hfsd-Token"

	// Exec frames are binary, tagged with a one-byte channel. An empty stdin
	// frame is EOF. The exit status arrives as a final text frame.
	ChanStdin  = 0
	ChanStdout = 1
	ChanStderr = 2
	// ChanResize carries a terminal size: rows then cols, big-endian uint16s.
	ChanResize = 3

	pingInterval = 30 * time.Second
	execChunk    = 32 * 1024
)

// ExecRequest is the first (text) frame a client sends on /exec.
type ExecRequest struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env,omitempty"`
	Dir  string            `json:"dir,omitempty"`

	// TTY runs the command on a pseudo-terminal: stderr is folded into
	// stdout, and the client may send resize frames.
	TTY  bool   `json:"tty,omitempty"`
	Term string `json:"term,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// ExecResult is the last (text) frame the server sends on /exec.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

type VersionInfo struct {
	SHA256 string `json:"sha256"`
}

// Remote returns the API served under RemotePrefix on the public port. It is
// remote code execution by design, and the Hub's own gate only proves read
// access to the Space, so every request must carry the hfsd token. Without a
// configured token the API doesn't exist.
func (s *Server) Remote() http.Handler {
	if s.Token == "" {
		return http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /exec", s.exec)
	mux.HandleFunc("PUT /files", s.putFile)
	mux.HandleFunc("GET /files", s.getFile)
	mux.HandleFunc("GET /version", s.version)
	mux.HandleFunc("PUT /upgrade", s.upgrade)
	mux.Handle("/api/", http.StripPrefix("/api", s.Control()))

	return http.StripPrefix(RemotePrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(TokenHeader)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, errors.New("missing or invalid "+TokenHeader))
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// exec runs one command for the lifetime of the websocket. If the client goes
// away the command is killed; work that should outlive its caller belongs in
// a job (`hfsd run`).
func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	_, first, err := c.Read(ctx)
	if err != nil {
		return
	}
	var req ExecRequest
	if err := json.Unmarshal(first, &req); err != nil || len(req.Argv) == 0 {
		c.Close(websocket.StatusUnsupportedData, "first frame must be an exec request with argv")
		return
	}

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	finish := func(res ExecResult) {
		b, _ := json.Marshal(res)
		c.Write(ctx, websocket.MessageText, b)
		c.Close(websocket.StatusNormalClosure, "")
	}

	var (
		stdin   io.WriteCloser
		outputs = map[byte]io.Reader{}
		ptmx    *os.File
		hangup  = syscall.SIGTERM
	)
	if req.TTY {
		if req.Term == "" {
			req.Term = "xterm-256color"
		}
		cmd.Env = append(cmd.Env, "TERM="+req.Term)
		// pty.Start makes the command a session leader on the new terminal,
		// so its pid doubles as the process group to signal.
		if req.Rows == 0 || req.Cols == 0 {
			req.Rows, req.Cols = 24, 80
		}
		ptmx, err = pty.StartWithSize(cmd, &pty.Winsize{Rows: req.Rows, Cols: req.Cols})
		if err == nil {
			defer ptmx.Close()
			stdin, outputs[ChanStdout], hangup = ptmx, ptmx, syscall.SIGHUP
		}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		stdin, _ = cmd.StdinPipe()
		outputs[ChanStdout], _ = cmd.StdoutPipe()
		outputs[ChanStderr], _ = cmd.StderrPipe()
		err = cmd.Start()
	}
	if err != nil {
		finish(ExecResult{ExitCode: 127, Error: err.Error()})
		return
	}

	// Writes to the socket come from both output pumps and must not interleave.
	var writeMu sync.Mutex
	pump := func(channel byte, src io.Reader) {
		buf := make([]byte, 1+execChunk)
		buf[0] = channel
		for {
			n, err := src.Read(buf[1:])
			if n > 0 {
				writeMu.Lock()
				werr := c.Write(ctx, websocket.MessageBinary, buf[:1+n])
				writeMu.Unlock()
				if werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	var pumps sync.WaitGroup
	for channel, src := range outputs {
		pumps.Add(1)
		go func() { defer pumps.Done(); pump(channel, src) }()
	}

	// Reading also services pongs, so this loop runs for the whole session.
	go func() {
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			if typ != websocket.MessageBinary || len(data) == 0 {
				continue
			}
			switch {
			case data[0] == ChanResize && ptmx != nil && len(data) == 5:
				pty.Setsize(ptmx, &pty.Winsize{
					Rows: binary.BigEndian.Uint16(data[1:3]),
					Cols: binary.BigEndian.Uint16(data[3:5]),
				})
			case data[0] != ChanStdin:
			case len(data) > 1:
				stdin.Write(data[1:])
			case ptmx == nil:
				// EOF. A terminal has no such thing; the user types ^D.
				stdin.Close()
			}
		}
	}()

	// Keeps idle sessions alive through proxies between us and the client.
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.Ping(ctx)
			}
		}
	}()

	go func() {
		<-ctx.Done()
		syscall.Kill(-cmd.Process.Pid, hangup)
		time.AfterFunc(5*time.Second, func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	}()

	pumps.Wait()
	cmd.Wait()
	if ctx.Err() != nil {
		return
	}
	finish(ExecResult{ExitCode: cmd.ProcessState.ExitCode()})
}

func (s *Server) putFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, errors.New("path must be absolute"))
		return
	}
	mode := os.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid mode %q", m))
			return
		}
		mode = os.FileMode(v)
	}
	sum, err := writeFileAtomic(path, mode, r.Body, r.Header.Get("X-Sha256"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, VersionInfo{SHA256: sum})
}

// writeFileAtomic streams src to a temp file next to path, verifies the
// checksum when one is given, and renames it into place.
func writeFileAtomic(path string, mode os.FileMode, src io.Reader, wantSum string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if wantSum != "" && wantSum != sum {
		return "", fmt.Errorf("checksum mismatch: got %s, want %s", sum, wantSum)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return "", err
	}
	return sum, os.Rename(tmp.Name(), path)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, errors.New("path must be absolute"))
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("not a regular file"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, f)
}

func selfSum() (string, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", err
	}
	return exe, hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	_, sum, err := selfSum()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, VersionInfo{SHA256: sum})
}

// upgrade replaces the running binary and re-execs into it. Services are
// stopped and come back from procs.json; a running job would be lost, so it
// blocks the upgrade unless forced.
func (s *Server) upgrade(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("force") == "" {
		for _, st := range s.Manager.List() {
			if st.Spec.Restart == RestartNever && !st.Terminal() {
				writeError(w, http.StatusConflict, fmt.Errorf("job %q is running; wait for it, stop it, or force the upgrade", st.Spec.Name))
				return
			}
		}
	}
	exe, _, err := selfSum()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// The persistent copy is what the entrypoint boots from next time.
	sum, err := writeFileAtomic(exe, 0o755, r.Body, r.Header.Get("X-Sha256"))
	if err == nil && s.Persist != "" {
		var src *os.File
		if src, err = os.Open(exe); err == nil {
			_, err = writeFileAtomic(s.Persist, 0o755, src, sum)
			src.Close()
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, VersionInfo{SHA256: sum})

	go func() {
		time.Sleep(200 * time.Millisecond)
		log.Printf("upgrading to %.12s", sum)
		s.Reexec <- exe
	}()
}
