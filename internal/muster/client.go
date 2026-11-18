// Package muster is the plugin side of muster's plugin protocol
// (https://github.com/ZacxDev/muster/blob/main/docs/plugins.md), cut down to
// the routes this plugin calls: GET /plugin/v1/self, GET /plugin/v1/tasks
// (once, at startup, to check the namespace) and POST /plugin/v1/tasks. The plugin calls muster for everything; muster never
// calls the plugin.
package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/textcut"
)

// TokenPrefix starts every plugin token.
const TokenPrefix = "mpl_"

// Kind classifies a refused or failed call by its HTTP status.
type Kind string

const (
	KindAuth        Kind = "auth"         // 401: not a live plugin token (revoked, unknown, owner disabled)
	KindCapability  Kind = "capability"   // 403: the token lacks the capability the route needs
	KindOff         Kind = "not_found"    // 404: plugins are off on this server
	KindBadRequest  Kind = "bad_request"  // 400 (and other 4xx)
	KindConflict    Kind = "conflict"     // 409
	KindRateLimited Kind = "rate_limited" // 429: see RetryAfter
	KindServer      Kind = "server"       // 5xx (503 included: see RetryAfter)
	KindNetwork     Kind = "network"      // no answer at all
	KindProtocol    Kind = "protocol"     // an answer this client does not understand
)

// KindForStatus maps an HTTP status to its Kind.
func KindForStatus(status int) Kind {
	switch {
	case status == http.StatusUnauthorized:
		return KindAuth
	case status == http.StatusForbidden:
		return KindCapability
	case status == http.StatusNotFound:
		return KindOff
	case status == http.StatusConflict:
		return KindConflict
	case status == http.StatusTooManyRequests:
		return KindRateLimited
	case status >= 500:
		return KindServer
	case status >= 400:
		return KindBadRequest
	}
	return KindProtocol
}

// Error is a call muster refused, or that got no answer.
type Error struct {
	Kind       Kind
	Status     int           // 0 for KindNetwork
	Message    string        // muster's {"error"} text, when it sent one
	RetryAfter time.Duration // from Retry-After (a 429 always has one; a 503 may)
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("muster: %s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("muster: %d %s: %s", e.Status, e.Kind, e.Message)
}

// Retryable says the same call is worth sending again.
func (e *Error) Retryable() bool {
	return e.Kind == KindNetwork || e.Kind == KindServer || e.Kind == KindRateLimited
}

// AsError returns err as an *Error, or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// CheckBaseURL validates muster's address: https, except plain http to a
// loopback host; no user, path, query or fragment (a trailing "/" is fine). A
// plugin token acts as its owner, so it never crosses a network in clear text,
// and a token pasted into the URL is refused rather than sent on every call.
func CheckBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		// Never echo the value: a mistaken one may hold the token.
		return "", errors.New("MUSTER_URL is not an absolute URL (its value is not shown, in case it holds a secret)")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && IsLoopbackHost(u.Hostname())) {
		return "", fmt.Errorf("MUSTER_URL must be https:// (http:// only for localhost, 127.0.0.1 or [::1])")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("MUSTER_URL must carry no user, query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("MUSTER_URL must be muster's address alone, with no path (e.g. https://muster.example.com)")
	}
	return u.Scheme + "://" + u.Host, nil
}

// IsLoopbackHost is localhost, 127.0.0.1 or ::1.
func IsLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// Redact is a URL cut to its scheme and host, for a log line or an error: a
// path, query or user part could hold the token. A value that does not parse
// is not shown at all.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(an address not shown)"
	}
	return u.Scheme + "://" + u.Host
}

// MaxErrorBytes bounds the text of a refused call kept in an Error (and so
// logged).
const MaxErrorBytes = 2 << 10

// capText cuts s to at most n bytes on a character boundary, marking the cut.
func capText(s string, n int) string { return textcut.Cap(s, n, " …[cut]") }

// Client calls muster's /plugin/v1 routes with one plugin token.
type Client struct {
	base  string
	token string
	HTTP  *http.Client
}

// New returns a Client. base must already pass CheckBaseURL.
func New(base, token string) (*Client, error) {
	if !strings.HasPrefix(token, TokenPrefix) || len(token) <= len(TokenPrefix) {
		return nil, fmt.Errorf("MUSTER_PLUGIN_TOKEN must start with %q", TokenPrefix)
	}
	return &Client{base: base, token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}

// SelfInfo is GET /plugin/v1/self's answer.
type SelfInfo struct {
	PluginID     int64    `json:"plugin_id"`
	PluginName   string   `json:"plugin_name"`
	OwnerLogin   string   `json:"owner_login"`
	Capabilities []string `json:"capabilities"`
}

// TaskCreate is the "create" part of a task request.
type TaskCreate struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Task is POST /plugin/v1/tasks' body for a COPY task (create and/or comment;
// docs/plugins.md, "POST /plugin/v1/tasks"). muster dedupes it by key (one
// open task per key) and by comment text (a comment is added once), and names
// the routed event by key and comment, so every comment this plugin sends
// starts with the upstream event's own id.
type Task struct {
	Key          string      `json:"key"`
	Create       *TaskCreate `json:"create,omitempty"`
	Comment      string      `json:"comment,omitempty"`
	RouteToChief bool        `json:"route_to_chief,omitempty"`
	Notice       string      `json:"notice,omitempty"`
}

// The notice kinds muster accepts with route_to_chief.
const (
	NoticeAssigned  = "assigned"
	NoticeMentioned = "mentioned"
	NoticeUpdate    = "update"
)

// TaskAck is muster's 2xx answer to a task request.
type TaskAck struct {
	ID        *int64          `json:"id"`
	Created   bool            `json:"created"`
	Commented bool            `json:"commented"`
	Routed    json.RawMessage `json:"routed,omitempty"`
}

// RoutedTo is where muster routed the notice ("chief", "agent", "attention"),
// or "" when it routed it nowhere (false) or was not asked to.
func (a TaskAck) RoutedTo() string {
	var s string
	if json.Unmarshal(a.Routed, &s) == nil {
		return s
	}
	return ""
}

// Self is GET /plugin/v1/self.
func (c *Client) Self(ctx context.Context) (SelfInfo, error) {
	var out SelfInfo
	_, err := c.do(ctx, http.MethodGet, "/plugin/v1/self", nil, &out)
	return out, err
}

// ProbeNamespace is GET /plugin/v1/tasks?namespace=<ns>&live=true, asked
// only for its status: muster answers 400 when ns is not one of its
// MUSTER_PLUGIN_TASK_NAMESPACES, and every task posted under it would then be
// refused the same way.
func (c *Client) ProbeNamespace(ctx context.Context, ns string) error {
	_, err := c.do(ctx, http.MethodGet, "/plugin/v1/tasks?"+url.Values{"namespace": {ns}, "live": {"true"}}.Encode(), nil, nil)
	return err
}

// PostTask is POST /plugin/v1/tasks. It is idempotent on muster's side, so
// the same request may always be sent again.
func (c *Client) PostTask(ctx context.Context, t Task) (TaskAck, error) {
	var out TaskAck
	_, err := c.do(ctx, http.MethodPost, "/plugin/v1/tasks", t, &out)
	return out, err
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// net/http's error names the whole request URL; keep its scheme and
		// host only.
		var ue *url.Error
		if errors.As(err, &ue) {
			ue.URL = Redact(ue.URL)
		}
		return 0, &Error{Kind: KindNetwork, Message: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return resp.StatusCode, &Error{Kind: KindProtocol, Status: resp.StatusCode, Message: "unreadable answer: " + err.Error()}
			}
		}
		return resp.StatusCode, nil
	}
	e := &Error{Kind: KindForStatus(resp.StatusCode), Status: resp.StatusCode}
	var msg struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &msg) == nil && msg.Error != "" {
		e.Message = capText(msg.Error, MaxErrorBytes)
	} else {
		// Not muster's JSON (a proxy's HTML page, say): logged, so bounded.
		e.Message = capText(strings.TrimSpace(string(raw)), MaxErrorBytes)
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		e.RetryAfter = time.Duration(s) * time.Second
	} else if e.Kind == KindRateLimited {
		e.RetryAfter = 60 * time.Second
	}
	return resp.StatusCode, e
}
