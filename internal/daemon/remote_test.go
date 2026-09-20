package daemon

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newRemote(t *testing.T, serverToken, clientToken string) *RemoteClient {
	t.Helper()
	srv := &Server{Manager: newTestManager(t), Upstream: "127.0.0.1:1", Token: serverToken}
	ts := httptest.NewServer(srv.Public())
	t.Cleanup(ts.Close)
	return NewRemoteClient(ts.URL, "hf-token", clientToken)
}

func TestExec(t *testing.T) {
	c := newRemote(t, "secret", "secret")
	var out, errOut bytes.Buffer

	code, err := c.Exec(context.Background(),
		ExecRequest{Argv: []string{"sh", "-c", "cat; echo oops >&2; exit 7"}, Env: map[string]string{"X": "1"}},
		strings.NewReader("from stdin"), &out, &errOut)
	if err != nil || code != 7 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if out.String() != "from stdin" || errOut.String() != "oops\n" {
		t.Errorf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestExecLargeStdin(t *testing.T) {
	c := newRemote(t, "secret", "secret")
	in := bytes.Repeat([]byte("0123456789abcdef"), 64*1024) // 1 MiB, like a pipelined module
	var out bytes.Buffer
	code, err := c.Exec(context.Background(), ExecRequest{Argv: []string{"cat"}}, bytes.NewReader(in), &out, &out)
	if err != nil || code != 0 || !bytes.Equal(out.Bytes(), in) {
		t.Fatalf("code=%d err=%v len=%d", code, err, out.Len())
	}
}

func TestExecMissingBinary(t *testing.T) {
	c := newRemote(t, "secret", "secret")
	code, err := c.Exec(context.Background(), ExecRequest{Argv: []string{"/nonexistent"}}, nil, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || code != 127 {
		t.Errorf("code=%d err=%v", code, err)
	}
}

func TestRemoteRequiresToken(t *testing.T) {
	for name, c := range map[string]*RemoteClient{
		"wrong token":    newRemote(t, "secret", "nope"),
		"no token":       newRemote(t, "secret", ""),
		"api turned off": newRemote(t, "", ""),
	} {
		if _, err := c.Exec(context.Background(), ExecRequest{Argv: []string{"true"}}, nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: exec succeeded", name)
		}
		if _, err := c.Version(context.Background()); err == nil {
			t.Errorf("%s: version succeeded", name)
		}
	}
}

func TestPutFetch(t *testing.T) {
	c := newRemote(t, "secret", "secret")
	dir := t.TempDir()
	src, dest, back := filepath.Join(dir, "src"), filepath.Join(dir, "sub", "dest"), filepath.Join(dir, "back")
	data := bytes.Repeat([]byte{0, 1, 2, 255}, 100_000)
	os.WriteFile(src, data, 0o644)

	if err := c.Put(context.Background(), src, dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dest); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("dest: %v %v", st, err)
	}
	if err := c.Fetch(context.Background(), dest, back); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(back); !bytes.Equal(got, data) {
		t.Error("round trip mismatch")
	}
}

func TestRemoteProcsAPI(t *testing.T) {
	c := newRemote(t, "secret", "secret")
	resp, err := c.request(context.Background(), "GET", "/api/procs", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestUpstreamPrefix(t *testing.T) {
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
	}))
	defer upstream.Close()

	srv := &Server{Manager: newTestManager(t), Upstream: strings.TrimPrefix(upstream.URL, "http://"), UpstreamPrefix: "/ui"}
	front := httptest.NewServer(srv.Public())
	defer front.Close()

	for _, path := range []string{"/v1/models", "/ui/", "/ui/v1/models", "/uix", "/health"} {
		resp, err := http.Get(front.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	want := []string{"/ui/v1/models", "/ui/", "/ui/v1/models", "/ui/uix", "/ui/health"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("upstream saw %v, want %v", got, want)
	}
}
