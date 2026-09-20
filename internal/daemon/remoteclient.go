package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/coder/websocket"
)

// RemoteClient talks to hfsd's remote API through the Space's public host.
type RemoteClient struct {
	base   string
	header http.Header
	http   *http.Client
}

// NewRemoteClient takes the Space's host (https://<subdomain>.hf.space), the
// HF token that gets a private Space past the Hub's gate, and the hfsd token.
func NewRemoteClient(host, hfToken, hfsdToken string) *RemoteClient {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+hfToken)
	h.Set(TokenHeader, hfsdToken)
	return &RemoteClient{
		base:   strings.TrimRight(host, "/") + RemotePrefix,
		header: h,
		http:   &http.Client{},
	}
}

func (c *RemoteClient) request(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Response, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header = c.header.Clone()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %w", method, path, remoteError(resp))
	}
	return resp, nil
}

// remoteError explains a failure. A non-JSON body means the response came
// from something in front of hfsd (the Hub's gate, nginx, llama-server).
func remoteError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	if resp.StatusCode == http.StatusNotFound {
		return errors.New("404: hfsd's remote api isn't there (not serving the app port, or no token configured)")
	}
	return errors.New(resp.Status)
}

// Exec runs argv on the Space, wiring up the given streams, and returns the
// command's exit code.
func (c *RemoteClient) Exec(ctx context.Context, req ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	conn, resp, err := websocket.Dial(ctx, c.base+"/exec", &websocket.DialOptions{HTTPHeader: c.header.Clone()})
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 {
			return -1, fmt.Errorf("exec: %w", remoteError(resp))
		}
		return -1, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	first, _ := json.Marshal(req)
	if err := conn.Write(ctx, websocket.MessageText, first); err != nil {
		return -1, err
	}

	go func() {
		buf := make([]byte, 1+execChunk)
		buf[0] = ChanStdin
		for stdin != nil {
			n, err := stdin.Read(buf[1:])
			if n > 0 && conn.Write(ctx, websocket.MessageBinary, buf[:1+n]) != nil {
				return
			}
			if err != nil {
				break
			}
		}
		conn.Write(ctx, websocket.MessageBinary, []byte{ChanStdin})
	}()

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return -1, fmt.Errorf("exec: connection lost: %w", err)
		}
		if typ == websocket.MessageText {
			var res ExecResult
			if err := json.Unmarshal(data, &res); err != nil {
				return -1, err
			}
			conn.Close(websocket.StatusNormalClosure, "")
			if res.Error != "" {
				return res.ExitCode, errors.New(res.Error)
			}
			return res.ExitCode, nil
		}
		if len(data) < 1 {
			continue
		}
		switch data[0] {
		case ChanStdout:
			stdout.Write(data[1:])
		case ChanStderr:
			stderr.Write(data[1:])
		}
	}
}

// Put uploads a local file, verified by checksum on the far side.
func (c *RemoteClient) Put(ctx context.Context, local, dest string, mode os.FileMode) error {
	b, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	_, err = c.send(ctx, "/files", url.Values{"path": {dest}, "mode": {fmt.Sprintf("%o", mode.Perm())}}, b)
	return err
}

func (c *RemoteClient) send(ctx context.Context, path string, query url.Values, b []byte) (string, error) {
	sum := sha256.Sum256(b)
	u := c.base + path + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header = c.header.Clone()
	req.Header.Set("X-Sha256", hex.EncodeToString(sum[:]))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("PUT %s: %w", path, remoteError(resp))
	}
	var v VersionInfo
	return v.SHA256, json.NewDecoder(resp.Body).Decode(&v)
}

// Fetch downloads a remote file.
func (c *RemoteClient) Fetch(ctx context.Context, src, local string) error {
	resp, err := c.request(ctx, http.MethodGet, "/files", url.Values{"path": {src}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(local)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Version returns the checksum of the running hfsd binary.
func (c *RemoteClient) Version(ctx context.Context) (string, error) {
	resp, err := c.request(ctx, http.MethodGet, "/version", nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v VersionInfo
	return v.SHA256, json.NewDecoder(resp.Body).Decode(&v)
}

// Upgrade replaces the running hfsd with the local binary if they differ, and
// reports whether it did. hfsd re-execs itself, so expect a brief outage.
func (c *RemoteClient) Upgrade(ctx context.Context, local string, force bool) (bool, error) {
	b, err := os.ReadFile(local)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(b)
	running, err := c.Version(ctx)
	if err != nil {
		return false, err
	}
	if running == hex.EncodeToString(sum[:]) {
		return false, nil
	}
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	_, err = c.send(ctx, "/upgrade", q, b)
	return err == nil, err
}
