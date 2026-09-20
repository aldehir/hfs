package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"
)

type applyRequest struct {
	Spec    Spec `json:"spec"`
	Replace bool `json:"replace"`
}

type applyResponse struct {
	Result string `json:"result"`
	Status Status `json:"status"`
}

type Server struct {
	Manager  *Manager
	Upstream string
	// Token guards the remote API; empty disables it.
	Token string
	// Persist is where an upgrade leaves a copy of the binary for the next boot.
	Persist string
	// Quit is closed by the shutdown endpoint.
	Quit chan struct{}
	// Reexec receives the path of a new binary to exec into.
	Reexec chan string
}

// Control returns the API served on the unix socket.
func (s *Server) Control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /procs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Manager.List())
	})
	mux.HandleFunc("POST /procs", func(w http.ResponseWriter, r *http.Request) {
		var req applyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		result, st, err := s.Manager.Apply(req.Spec, req.Replace)
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, applyResponse{Result: result, Status: st})
	})
	mux.HandleFunc("GET /procs/{name}", func(w http.ResponseWriter, r *http.Request) {
		st, err := s.Manager.Status(r.PathValue("name"))
		respond(w, st, err)
	})
	mux.HandleFunc("GET /procs/{name}/wait", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		done, err := s.Manager.Done(name)
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		select {
		case <-done:
		case <-r.Context().Done():
			return
		}
		st, err := s.Manager.Status(name)
		respond(w, st, err)
	})
	mux.HandleFunc("POST /procs/{name}/{action}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var (
			st  Status
			err error
		)
		switch r.PathValue("action") {
		case "stop":
			st, err = s.Manager.Stop(name)
		case "start":
			st, err = s.Manager.Start(name, false)
		case "restart":
			st, err = s.Manager.Start(name, true)
		default:
			http.NotFound(w, r)
			return
		}
		respond(w, st, err)
	})
	mux.HandleFunc("DELETE /procs/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Manager.Remove(r.PathValue("name")); err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /procs/{name}/logs", s.logs)
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		close(s.Quit)
	})
	return mux
}

// logs streams a process's log. With follow it keeps going until the process
// is finished and fully read, or the client goes away.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.Manager.Status(name); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	follow := r.URL.Query().Get("follow") != ""

	f, err := os.Open(s.Manager.LogPath(name))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	defer f.Close()
	if tail > 0 {
		seekTail(f, tail)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rc := http.NewResponseController(w)
	for {
		// Check before copying so output written just before exit isn't missed.
		st, err := s.Manager.Status(name)
		finished := err != nil || st.Terminal()
		n, _ := io.Copy(w, f)
		rc.Flush()
		if !follow || (finished && n == 0) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// seekTail positions f at the start of its last n lines.
func seekTail(f *os.File, n int) {
	const window = 1 << 20
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return
	}
	start := max(size-window, 0)
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		f.Seek(0, io.SeekStart)
		return
	}
	pos := len(buf)
	if pos > 0 && buf[pos-1] == '\n' {
		pos--
	}
	for ; n > 0 && pos >= 0; n-- {
		pos = bytes.LastIndexByte(buf[:pos], '\n')
	}
	f.Seek(start+int64(pos)+1, io.SeekStart)
}

// Public returns the Space's front door: a reverse proxy to llama-server that
// degrades to a status page, so the Space always answers its health check.
func (s *Server) Public() http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: s.Upstream})
	proxy.FlushInterval = -1
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("X-Upstream-Status", "offline")
		s.statusPage(w)
	}

	// llama-server has no business seeing the hfsd token.
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.Header.Del(TokenHeader)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.statusPage(w) })
	mux.Handle(RemotePrefix+"/", s.Remote())
	mux.Handle("/", proxy)
	return mux
}

func (s *Server) statusPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<!doctype html><title>hfsd</title><style>body{font:14px monospace;margin:2em}td{padding-right:2em}</style><h3>hfsd</h3><table>")
	for _, st := range s.Manager.List() {
		fmt.Fprintf(w, "<tr><td>%s<td>%s", html.EscapeString(st.Spec.Name), st.State)
	}
	fmt.Fprint(w, "</table>")
}

// ListenUnix listens on a unix socket, replacing a stale one.
func ListenUnix(path string) (net.Listener, error) {
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, fmt.Errorf("hfsd is already running on %s", path)
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return l, os.Chmod(path, 0o600)
}

func Shutdown(ctx context.Context, servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		srv.Shutdown(ctx)
	}
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func respond(w http.ResponseWriter, st Status, err error) {
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
