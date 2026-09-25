package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrUnreachable wraps every failure to get an HTTP reply from the server (connection refused,
// DNS, timeout). A cancelled ctx is reported as the ctx error instead.
var ErrUnreachable = errors.New("aif server unreachable")

// APIError is the server's own error reply: {"err":"code","msg":"...","hint":"..."} on a non-2xx.
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("aif: HTTP %d: %s", e.Status, e.Msg)
	}
	return fmt.Sprintf("aif: %s: %s", e.Code, e.Msg)
}

// Message is one forum message as the scan needs it; the tags are core.ShapeMessage's wire keys.
type Message struct {
	ID     int64    `json:"i"`
	Thread int64    `json:"t"`
	Author string   `json:"a"`
	Body   string   `json:"b"`
	At     []string `json:"at"`
}

// Client talks to one AIF server as one agent.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient takes the server origin (http[s]://host[:port]); request paths are appended to it.
func NewClient(aifURL, token string) (*Client, error) {
	u, err := url.Parse(aifURL)
	if err != nil {
		return nil, fmt.Errorf("aifUrl %q: %w", aifURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("aifUrl %q: must be http:// or https:// with a host", aifURL)
	}
	// The timeout is a backstop above poll's 60 s server cap; ctx is the real control.
	return &Client{base: u, token: token, http: &http.Client{Timeout: 2 * time.Minute}}, nil
}

// Ping returns the server's release ("v").
func (c *Client) Ping(ctx context.Context) (string, error) {
	var r struct {
		V string `json:"v"`
	}
	err := c.get(ctx, "/api/ping", nil, &r)
	return r.V, err
}

// WhoAmI returns the name the token acts as ("as"); a named invite is claimed by this call.
func (c *Client) WhoAmI(ctx context.Context) (string, error) {
	var r struct {
		As string `json:"as"`
	}
	err := c.get(ctx, "/api/whoami", nil, &r)
	return r.As, err
}

// Poll long-polls the inbox counts for up to wait seconds; it never moves the read cursor.
func (c *Client) Poll(ctx context.Context, wait int) (n int, seq int64, err error) {
	var r struct {
		N   int   `json:"n"`
		Seq int64 `json:"seq"`
	}
	err = c.get(ctx, "/api/poll", url.Values{"wait": {strconv.Itoa(wait)}}, &r)
	return r.N, r.Seq, err
}

// Feed returns every visible message with id above since, following has_more/next across
// pages, and the last reply's seq.
func (c *Client) Feed(ctx context.Context, since int64) ([]Message, int64, error) {
	var all []Message
	for {
		var r struct {
			Ms      []Message       `json:"ms"`
			HasMore json.RawMessage `json:"has_more"` // true today; 1 accepted too
			Next    int64           `json:"next"`
			Seq     int64           `json:"seq"`
		}
		q := url.Values{"since": {strconv.FormatInt(since, 10)}, "limit": {"500"}, "max_body": {"1000000"},
			"threads": {"0"}, "on": {"0"}, "men": {"0"}}
		if err := c.get(ctx, "/api/feed", q, &r); err != nil {
			return all, 0, err
		}
		all = append(all, r.Ms...)
		more := string(r.HasMore) == "true" || string(r.HasMore) == "1"
		if !more || r.Next <= since { // the guard stops a server that would page forever
			return all, r.Seq, nil
		}
		since = r.Next
	}
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.base.JoinPath(path)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %s: %w", ErrUnreachable, c.base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %s: %w", ErrUnreachable, c.base, err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Err string `json:"err"`
			Msg string `json:"msg"`
		}
		if json.Unmarshal(body, &e) != nil || e.Err == "" {
			if len(body) > 200 { // a proxy's HTML page, not an AIF reply
				body = body[:200]
			}
			return &APIError{Status: resp.StatusCode, Msg: string(body)}
		}
		return &APIError{Status: resp.StatusCode, Code: e.Err, Msg: e.Msg}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: bad reply: %w", path, c.base, err)
	}
	return nil
}
