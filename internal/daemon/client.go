package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

// Client talks to hfsd over its unix socket.
type Client struct {
	http *http.Client
}

func NewClient(socket string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://hfsd"+path, r)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hfsd not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	return errors.New(resp.Status)
}

func (c *Client) Apply(ctx context.Context, spec Spec, replace bool) (string, Status, error) {
	var resp applyResponse
	err := c.do(ctx, http.MethodPost, "/procs", applyRequest{Spec: spec, Replace: replace}, &resp)
	return resp.Result, resp.Status, err
}

func (c *Client) List(ctx context.Context) ([]Status, error) {
	var out []Status
	return out, c.do(ctx, http.MethodGet, "/procs", nil, &out)
}

func (c *Client) Status(ctx context.Context, name string) (Status, error) {
	var st Status
	return st, c.do(ctx, http.MethodGet, "/procs/"+url.PathEscape(name), nil, &st)
}

// Wait blocks until the process reaches a terminal state.
func (c *Client) Wait(ctx context.Context, name string) (Status, error) {
	var st Status
	return st, c.do(ctx, http.MethodGet, "/procs/"+url.PathEscape(name)+"/wait", nil, &st)
}

// Action performs stop, start, or restart.
func (c *Client) Action(ctx context.Context, name, action string) (Status, error) {
	var st Status
	return st, c.do(ctx, http.MethodPost, "/procs/"+url.PathEscape(name)+"/"+action, nil, &st)
}

func (c *Client) Remove(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/procs/"+url.PathEscape(name), nil, nil)
}

func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/shutdown", nil, nil)
}

func (c *Client) Logs(ctx context.Context, name string, tail int, follow bool, w io.Writer) error {
	q := url.Values{"tail": {strconv.Itoa(tail)}}
	if follow {
		q.Set("follow", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://hfsd/procs/"+url.PathEscape(name)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hfsd not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}
	_, err = io.Copy(w, resp.Body)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
