package daemon

import (
	"os"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

func wait(t *testing.T, m *Manager, name string) Status {
	t.Helper()
	done, err := m.Done(name)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not finish", name)
	}
	st, _ := m.Status(name)
	return st
}

func TestJobExitStatus(t *testing.T) {
	m := newTestManager(t)

	m.Apply(Spec{Name: "ok", Command: []string{"sh", "-c", "echo hi"}}, false)
	if st := wait(t, m, "ok"); st.State != StateExited || *st.ExitCode != 0 {
		t.Errorf("ok: got %s code=%v", st.State, st.ExitCode)
	}
	if b, _ := os.ReadFile(m.LogPath("ok")); strings.TrimSpace(string(b)) != "hi" {
		t.Errorf("log = %q", b)
	}

	m.Apply(Spec{Name: "bad", Command: []string{"sh", "-c", "exit 3"}}, false)
	if st := wait(t, m, "bad"); st.State != StateFailed || *st.ExitCode != 3 {
		t.Errorf("bad: got %s code=%v", st.State, st.ExitCode)
	}

	m.Apply(Spec{Name: "missing", Command: []string{"/nonexistent"}}, false)
	if st := wait(t, m, "missing"); st.State != StateFailed || st.Error == "" {
		t.Errorf("missing: got %s err=%q", st.State, st.Error)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	m := newTestManager(t)
	spec := Spec{Name: "svc", Command: []string{"sleep", "60"}, Restart: RestartAlways}

	result, first, err := m.Apply(spec, false)
	if err != nil || result != "started" {
		t.Fatalf("first apply: %q, %v", result, err)
	}
	time.Sleep(100 * time.Millisecond)
	first, _ = m.Status("svc")

	result, same, _ := m.Apply(spec, false)
	if result != "unchanged" || same.PID != first.PID {
		t.Errorf("same spec: %q pid %d -> %d", result, first.PID, same.PID)
	}

	changed := spec
	changed.Command = []string{"sleep", "61"}
	if _, _, err := m.Apply(changed, false); err != ErrConflict {
		t.Errorf("changed spec without replace: err = %v", err)
	}
	if result, _, err := m.Apply(changed, true); err != nil || result != "replaced" {
		t.Errorf("changed spec with replace: %q, %v", result, err)
	}
}

func TestStopKillsProcessGroup(t *testing.T) {
	m := newTestManager(t)
	m.Apply(Spec{Name: "tree", Command: []string{"sh", "-c", "sleep 60 & wait"}, Restart: RestartAlways}, false)
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	st, err := m.Stop("tree")
	if err != nil || st.State != StateStopped {
		t.Fatalf("stop: %s, %v", st.State, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("stop took %s", time.Since(start))
	}
}

func TestRestoreRelaunchesServices(t *testing.T) {
	home := t.TempDir()
	m1, _ := NewManager(home)
	m1.Apply(Spec{Name: "svc", Command: []string{"sleep", "60"}, Restart: RestartAlways}, false)
	m1.Apply(Spec{Name: "job", Command: []string{"true"}}, false)
	m1.Shutdown()

	m2, _ := NewManager(home)
	t.Cleanup(m2.Shutdown)
	if err := m2.Restore(); err != nil {
		t.Fatal(err)
	}
	if list := m2.List(); len(list) != 1 || list[0].Spec.Name != "svc" {
		t.Errorf("restored %+v", list)
	}
}
