// Package hf is a thin client for the Hugging Face Spaces REST API.
//
// A Client is bound to exactly one Space at construction; every request URL is
// derived from that ID, so it cannot touch anything else.
package hf

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const endpoint = "https://huggingface.co"

var spaceIDPattern = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)

type Client struct {
	space string
	token string
	http  *http.Client
}

type Hardware struct {
	Current   string `json:"current"`
	Requested string `json:"requested"`
}

type Runtime struct {
	Stage        string   `json:"stage"`
	Hardware     Hardware `json:"hardware"`
	SleepTime    int      `json:"gcTimeout"`
	DevMode      bool     `json:"devMode"`
	SHA          string   `json:"sha"`
	ErrorMessage string   `json:"errorMessage"`
}

type Info struct {
	ID        string  `json:"id"`
	Private   bool    `json:"private"`
	Subdomain string  `json:"subdomain"`
	Host      string  `json:"host"`
	Runtime   Runtime `json:"runtime"`
}

type LogLine struct {
	Data      string `json:"data"`
	Timestamp string `json:"timestamp"`
}

func New(space, token string) (*Client, error) {
	if !spaceIDPattern.MatchString(space) {
		return nil, fmt.Errorf("invalid space id %q, want NAMESPACE/NAME", space)
	}
	if token == "" {
		return nil, errors.New("no HF token: set $HF_TOKEN or run `hf auth login`")
	}
	return &Client{space: space, token: token, http: &http.Client{}}, nil
}

// Token returns $HF_TOKEN, falling back to the token file written by the hf CLI.
func Token() string {
	if t := os.Getenv("HF_TOKEN"); t != "" {
		return t
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".cache", "huggingface", "token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (c *Client) Space() string { return c.space }

func (c *Client) url(suffix string) string {
	return endpoint + "/api/spaces/" + c.space + suffix
}

func (c *Client) do(ctx context.Context, method, suffix string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(suffix), r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := c.http.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return fmt.Errorf("%s %s: %s: %s", method, suffix, resp.Status, msg)
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) Info(ctx context.Context) (*Info, error) {
	var i Info
	return &i, c.do(ctx, http.MethodGet, "", nil, &i)
}

func (c *Client) Runtime(ctx context.Context) (*Runtime, error) {
	var r Runtime
	return &r, c.do(ctx, http.MethodGet, "/runtime", nil, &r)
}

func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/pause", nil, nil)
}

func (c *Client) Restart(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/restart", nil, nil)
}

// SetSleepTime sets the idle timeout in seconds; -1 means never sleep.
func (c *Client) SetSleepTime(ctx context.Context, seconds int) error {
	return c.do(ctx, http.MethodPost, "/sleeptime", map[string]int{"seconds": seconds}, nil)
}

func (c *Client) RequestHardware(ctx context.Context, flavor string) error {
	return c.do(ctx, http.MethodPost, "/hardware", map[string]string{"flavor": flavor}, nil)
}

func (c *Client) SetDevMode(ctx context.Context, enabled bool) error {
	return c.do(ctx, http.MethodPost, "/dev-mode", map[string]bool{"enabled": enabled}, nil)
}

// Logs streams the Space's build or run logs until ctx is cancelled or the
// server closes the stream.
func (c *Client) Logs(ctx context.Context, build bool, fn func(LogLine)) error {
	kind := "run"
	if build {
		kind = "build"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/logs/"+kind), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET /logs/%s: %s", kind, resp.Status)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var l LogLine
		if json.Unmarshal([]byte(data), &l) == nil {
			fn(l)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// Get performs an authenticated GET against the Space's own host (the app,
// not the Hub API). Private Spaces accept the HF token as a bearer token.
func (c *Client) Get(ctx context.Context, host, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(host, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.http.Do(req)
}
