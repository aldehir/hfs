// Package daemon implements hfsd: a process manager and HTTP front door that
// runs inside the Space.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	RestartNever     = "never"
	RestartOnFailure = "on-failure"
	RestartAlways    = "always"

	StateRunning = "running"
	StateBackoff = "backoff"
	StateExited  = "exited"
	StateFailed  = "failed"
	StateStopped = "stopped"

	stopTimeout = 30 * time.Second
	// A process that dies this quickly, this many times in a row, is given up on.
	quickExit    = 10 * time.Second
	maxQuickExit = 5
)

var (
	ErrNotFound = errors.New("no such process")
	ErrConflict = errors.New("a process with this name is running with a different spec (use --replace)")

	namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

// Spec describes a process. Restart "never" makes it a one-shot job; anything
// else makes it a service.
type Spec struct {
	Name    string            `json:"name"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Dir     string            `json:"dir,omitempty"`
	Restart string            `json:"restart,omitempty"`
}

type Status struct {
	Spec      Spec       `json:"spec"`
	State     string     `json:"state"`
	PID       int        `json:"pid,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Error     string     `json:"error,omitempty"`
	Restarts  int        `json:"restarts"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	ExitedAt  *time.Time `json:"exited_at,omitempty"`
}

func (s Status) Terminal() bool {
	return s.State == StateExited || s.State == StateFailed || s.State == StateStopped
}

func (s *Spec) validate() error {
	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("invalid name %q", s.Name)
	}
	if len(s.Command) == 0 {
		return errors.New("command is required")
	}
	switch s.Restart {
	case "":
		s.Restart = RestartNever
	case RestartNever, RestartOnFailure, RestartAlways:
	default:
		return fmt.Errorf("invalid restart policy %q", s.Restart)
	}
	if len(s.Env) == 0 {
		s.Env = nil
	}
	return nil
}

type proc struct {
	logPath string

	mu       sync.Mutex
	status   Status
	stopping bool

	stopCh chan struct{}
	done   chan struct{}
}

func (p *proc) snapshot() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *proc) supervise() {
	defer close(p.done)
	spec := p.status.Spec
	quick := 0
	for {
		started := time.Now()
		code, err := p.runOnce()
		now := time.Now()

		p.mu.Lock()
		p.status.PID = 0
		p.status.ExitedAt = &now
		p.status.ExitCode = &code
		p.status.Error = ""
		if err != nil {
			p.status.Error = err.Error()
		}
		if p.stopping {
			p.status.State = StateStopped
			p.mu.Unlock()
			return
		}

		failed := err != nil || code != 0
		if spec.Restart == RestartAlways || (spec.Restart == RestartOnFailure && failed) {
			if now.Sub(started) < quickExit {
				quick++
			} else {
				quick = 0
			}
			if quick >= maxQuickExit {
				p.status.State = StateFailed
				p.status.Error = fmt.Sprintf("exited too quickly %d times in a row", quick)
				p.mu.Unlock()
				return
			}
			p.status.State = StateBackoff
			p.status.Restarts++
			p.mu.Unlock()

			select {
			case <-time.After(min(time.Duration(1<<quick)*time.Second, 30*time.Second)):
				continue
			case <-p.stopCh:
				p.mu.Lock()
				p.status.State = StateStopped
				p.mu.Unlock()
				return
			}
		}

		p.status.State = StateExited
		if failed {
			p.status.State = StateFailed
		}
		p.mu.Unlock()
		return
	}
}

// runOnce runs the command to completion and returns its exit code. A non-nil
// error means the process could not be started.
func (p *proc) runOnce() (int, error) {
	spec := p.status.Spec

	logf, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return -1, err
	}
	defer logf.Close()

	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	// Own process group, so stop can signal the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logf, "hfsd: failed to start: %v\n", err)
		return -1, err
	}
	now := time.Now()
	p.mu.Lock()
	p.status.State = StateRunning
	p.status.PID = cmd.Process.Pid
	p.status.StartedAt = &now
	p.status.ExitedAt = nil
	p.status.ExitCode = nil
	p.mu.Unlock()

	exited := make(chan struct{})
	go func() {
		select {
		case <-exited:
			return
		case <-p.stopCh:
		}
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(stopTimeout):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	cmd.Wait()
	close(exited)
	return cmd.ProcessState.ExitCode(), nil
}

// stop asks the process to exit and waits until it has.
func (p *proc) stop() {
	p.mu.Lock()
	if !p.stopping {
		p.stopping = true
		close(p.stopCh)
	}
	p.mu.Unlock()
	<-p.done
}

type Manager struct {
	home string

	// applyMu serializes mutations; mu only guards the map.
	applyMu sync.Mutex
	mu      sync.RWMutex
	procs   map[string]*proc
}

func NewManager(home string) (*Manager, error) {
	if err := os.MkdirAll(filepath.Join(home, "logs"), 0o755); err != nil {
		return nil, err
	}
	return &Manager{home: home, procs: map[string]*proc{}}, nil
}

func (m *Manager) LogPath(name string) string {
	return filepath.Join(m.home, "logs", name+".log")
}

func (m *Manager) specsPath() string { return filepath.Join(m.home, "procs.json") }

func (m *Manager) get(name string) (*proc, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.procs[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return p, nil
}

// Apply starts spec. If a process with the same name and spec is already
// live it is left alone, which makes re-running a provision idempotent and
// lets a caller re-attach to a job after losing its connection.
func (m *Manager) Apply(spec Spec, replace bool) (string, Status, error) {
	if err := spec.validate(); err != nil {
		return "", Status{}, err
	}
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	result := "started"
	if old, err := m.get(spec.Name); err == nil {
		if st := old.snapshot(); !st.Terminal() {
			if reflect.DeepEqual(st.Spec, spec) {
				return "unchanged", st, nil
			}
			if !replace {
				return "", st, ErrConflict
			}
			old.stop()
			result = "replaced"
		}
	}
	return result, m.launch(spec, true), nil
}

func (m *Manager) launch(spec Spec, rotate bool) Status {
	p := &proc{
		logPath: m.LogPath(spec.Name),
		status:  Status{Spec: spec, State: StateRunning},
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	if rotate {
		os.Rename(p.logPath, p.logPath+".1")
	}
	m.mu.Lock()
	m.procs[spec.Name] = p
	m.mu.Unlock()
	m.persist()
	go p.supervise()
	return p.snapshot()
}

func (m *Manager) Stop(name string) (Status, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	p, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	if !p.snapshot().Terminal() {
		p.stop()
	}
	return p.snapshot(), nil
}

// Start runs a finished process again with its existing spec.
func (m *Manager) Start(name string, restart bool) (Status, error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	p, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	if st := p.snapshot(); !st.Terminal() {
		if !restart {
			return st, nil
		}
		p.stop()
	}
	return m.launch(p.snapshot().Spec, true), nil
}

func (m *Manager) Remove(name string) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	p, err := m.get(name)
	if err != nil {
		return err
	}
	if !p.snapshot().Terminal() {
		p.stop()
	}
	m.mu.Lock()
	delete(m.procs, name)
	m.mu.Unlock()
	m.persist()
	return nil
}

func (m *Manager) Status(name string) (Status, error) {
	p, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	return p.snapshot(), nil
}

// Done returns a channel that closes when the process reaches a terminal state.
func (m *Manager) Done(name string) (<-chan struct{}, error) {
	p, err := m.get(name)
	if err != nil {
		return nil, err
	}
	return p.done, nil
}

func (m *Manager) List() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.procs))
	for _, p := range m.procs {
		out = append(out, p.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out
}

// persist records service specs so they come back after hfsd restarts.
func (m *Manager) persist() {
	var specs []Spec
	for _, st := range m.List() {
		if st.Spec.Restart != RestartNever {
			specs = append(specs, st.Spec)
		}
	}
	b, _ := json.MarshalIndent(specs, "", "  ")
	tmp := m.specsPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, m.specsPath())
	}
}

// Restore launches the services recorded by a previous hfsd.
func (m *Manager) Restore() error {
	b, err := os.ReadFile(m.specsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var specs []Spec
	if err := json.Unmarshal(b, &specs); err != nil {
		return err
	}
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	for _, s := range specs {
		if s.validate() == nil {
			m.launch(s, false)
		}
	}
	return nil
}

// Shutdown stops every process.
func (m *Manager) Shutdown() {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	m.mu.RLock()
	procs := make([]*proc, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	m.mu.RUnlock()

	var wg sync.WaitGroup
	for _, p := range procs {
		if p.snapshot().Terminal() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.stop()
		}()
	}
	wg.Wait()
}
