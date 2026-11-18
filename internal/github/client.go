// Package github is the small part of GitHub's REST API this plugin reads:
// a repository, its issues (updated since a time), and an issue's comments.
// It only ever sends GET requests.
package github

import (
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

// DefaultAPI is github.com's API.
const DefaultAPI = "https://api.github.com"

// Repo is owner/name.
type Repo struct{ Owner, Name string }

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// ParseRepo reads "owner/name".
func ParseRepo(s string) (Repo, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(s), "/")
	valid := func(v string) bool {
		if v == "" || len(v) > 100 || v == "." || v == ".." {
			return false
		}
		for _, c := range v {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return false
			}
		}
		return true
	}
	if !ok || !valid(owner) || !valid(name) {
		return Repo{}, fmt.Errorf("repository %q is not owner/name", s)
	}
	return Repo{owner, name}, nil
}

// Label is an issue label.
type Label struct {
	Name string `json:"name"`
}

// User is an issue's or comment's author.
type User struct {
	Login string `json:"login"`
}

// Issue is the part of a GitHub issue this plugin reads.
type Issue struct {
	Number      int              `json:"number"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	HTMLURL     string           `json:"html_url"`
	State       string           `json:"state"` // "open" or "closed"
	StateReason string           `json:"state_reason"`
	Labels      []Label          `json:"labels"`
	User        User             `json:"user"`
	Comments    int              `json:"comments"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	PullRequest *json.RawMessage `json:"pull_request"` // set: the "issue" is a pull request
}

// LabelNames lists the issue's label names.
func (i Issue) LabelNames() []string {
	out := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	return out
}

// Comment is an issue comment.
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Error is a request GitHub refused or did not answer.
type Error struct {
	Status  int    // 0: no answer
	Message string // GitHub's "message", or the transport error
	// RetryAt is set when GitHub said to wait (a rate limit): not before then.
	RetryAt time.Time
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return "github: " + e.Message
	}
	return fmt.Sprintf("github: %d: %s", e.Status, e.Message)
}

// Unauthorized says the token is not accepted (401): no retry fixes it.
func (e *Error) Unauthorized() bool { return e.Status == http.StatusUnauthorized }

// AsError returns err as an *Error, or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// Client reads GitHub's REST API with one token.
type Client struct {
	base  string
	token string
	HTTP  *http.Client
	Now   func() time.Time
}

// CheckAPIURL validates GITHUB_API_URL: https, or http to a loopback host (a
// test or a local proxy); no user, query or fragment.
func CheckAPIURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", errors.New("GITHUB_API_URL is not an absolute URL")
	}
	h := u.Hostname()
	loop := h == "localhost" || h == "127.0.0.1" || h == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loop) {
		return "", errors.New("GITHUB_API_URL must be https:// (http:// only to localhost)")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("GITHUB_API_URL must carry no user, query or fragment")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// New returns a Client. base must already pass CheckAPIURL.
func New(base, token string) *Client {
	return &Client{base: base, token: token, HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now}
}

func (c *Client) get(ctx context.Context, rawURL string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "muster-github-issues")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is logged elsewhere without the query
		}
		return nil, &Error{Message: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, &Error{Status: resp.StatusCode, Message: "unreadable answer: " + err.Error()}
		}
		return resp.Header, nil
	}
	e := &Error{Status: resp.StatusCode}
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &msg) == nil && msg.Message != "" {
		e.Message = capText(msg.Message, MaxErrorBytes)
	} else {
		// Not GitHub's JSON (a proxy's HTML page, say): logged, so bounded.
		e.Message = capText(strings.TrimSpace(string(raw)), MaxErrorBytes)
	}
	// Rate limits (docs.github.com, "Rate limits for the REST API"): a 403 or
	// 429 with Retry-After, or with x-ratelimit-remaining 0 and a reset time.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			e.RetryAt = c.Now().Add(time.Duration(s) * time.Second)
		} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			if r, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				e.RetryAt = time.Unix(r, 0)
			}
		} else if resp.StatusCode == http.StatusTooManyRequests {
			e.RetryAt = c.Now().Add(time.Minute)
		}
	}
	return nil, e
}

// MaxErrorBytes bounds the text of a refused request kept in an Error (and so
// logged).
const MaxErrorBytes = 2 << 10

// capText cuts s to at most n bytes on a character boundary, marking the cut.
func capText(s string, n int) string { return textcut.Cap(s, n, " …[cut]") }

// CheckRepo is GET /repos/{owner}/{name}: does the token see it.
func (c *Client) CheckRepo(ctx context.Context, r Repo) error {
	var out struct {
		FullName string `json:"full_name"`
	}
	_, err := c.get(ctx, c.base+"/repos/"+url.PathEscape(r.Owner)+"/"+url.PathEscape(r.Name), &out)
	return err
}

// IssuesPage is GET /repos/{owner}/{name}/issues: every issue (and pull
// request: GitHub lists both) updated at or after since, oldest update first,
// 100 a page. next is the following page's URL, or "".
func (c *Client) IssuesPage(ctx context.Context, r Repo, since time.Time, pageURL string) (issues []Issue, next string, err error) {
	if pageURL == "" {
		q := url.Values{}
		q.Set("state", "all")
		q.Set("sort", "updated")
		q.Set("direction", "asc")
		q.Set("per_page", "100")
		q.Set("since", since.UTC().Format(time.RFC3339))
		pageURL = c.base + "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name) + "/issues?" + q.Encode()
	} else if !strings.HasPrefix(pageURL, c.base+"/") {
		// A Link header pointing anywhere else would carry the token there.
		return nil, "", &Error{Message: "the next-page link is not on GITHUB_API_URL"}
	}
	h, err := c.get(ctx, pageURL, &issues)
	if err != nil {
		return nil, "", err
	}
	return issues, nextLink(h.Get("Link")), nil
}

// Comments is GET /repos/{owner}/{name}/issues/{n}/comments since a time,
// oldest first. GitHub answers that route oldest first only (it takes no sort
// or direction), so every page is read: the newest comments are on the last
// one. It returns the newest keep comments and how many there were in all.
func (c *Client) Comments(ctx context.Context, r Repo, number int, since time.Time, keep int) (newest []Comment, total int, err error) {
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("since", since.UTC().Format(time.RFC3339))
	pageURL := c.base + "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name) + "/issues/" + strconv.Itoa(number) + "/comments?" + q.Encode()
	for pageURL != "" {
		if !strings.HasPrefix(pageURL, c.base+"/") {
			return nil, 0, &Error{Message: "the next-page link is not on GITHUB_API_URL"}
		}
		var page []Comment
		h, err := c.get(ctx, pageURL, &page)
		if err != nil {
			return nil, 0, err
		}
		total += len(page)
		newest = append(newest, page...)
		if len(newest) > keep {
			newest = newest[len(newest)-keep:]
		}
		pageURL = nextLink(h.Get("Link"))
	}
	return newest, total, nil
}

// nextLink reads rel="next" from a Link header.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		seg := strings.Split(part, ";")
		if len(seg) < 2 {
			continue
		}
		for _, p := range seg[1:] {
			if strings.TrimSpace(p) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(seg[0]), "<>")
			}
		}
	}
	return ""
}
