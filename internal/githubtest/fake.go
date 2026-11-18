// Package githubtest is a fake GitHub REST API for tests: a repository, its
// issues (filtered by since, sorted by update, paginated with a Link header)
// and their comments, answering the way api.github.com does, and recording
// every request.
package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/github"
)

// Token is the GitHub token the fake accepts.
const Token = "github_pat_test_only_not_a_real_token"

// Fake is a fake GitHub.
type Fake struct {
	*httptest.Server

	mu       sync.Mutex
	issues   map[string][]github.Issue   // "owner/name" → issues
	comments map[string][]github.Comment // "owner/name#n" → comments
	// PerPage is the page size (GitHub's per_page; default 100).
	PerPage int
	// RateLimited answers the next n issue-list requests 403 with
	// x-ratelimit-remaining 0, as GitHub does.
	RateLimited int
	// RateLimitedLater answers the next n issue-list requests after the
	// first one the fake ever served the same way: a poll cut off mid-way.
	RateLimitedLater int
	// CommentsPerPage is the comment list's page size (default 100).
	CommentsPerPage int
	// OnList, when set, runs before the fake answers an issue-list request,
	// with that request's number (1, 2, …) and outside the fake's lock: it
	// may Put, as an update landing mid-poll does.
	OnList func(n int)
	lists  int
	// Methods is every request's method and path.
	Methods []string
}

// New starts a fake GitHub serving repo.
func New(repos ...string) *Fake {
	f := &Fake{issues: map[string][]github.Issue{}, comments: map[string][]github.Comment{}, PerPage: 100, CommentsPerPage: 100}
	for _, r := range repos {
		f.issues[r] = nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}", f.gate(f.repo))
	mux.HandleFunc("GET /repos/{owner}/{name}/issues", f.gate(f.list))
	mux.HandleFunc("GET /repos/{owner}/{name}/issues/{n}/comments", f.gate(f.listComments))
	f.Server = httptest.NewServer(mux)
	return f
}

// Put adds or replaces an issue (matched by number).
func (f *Fake) Put(repo string, i github.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i.HTMLURL == "" {
		i.HTMLURL = fmt.Sprintf("https://github.example.com/%s/issues/%d", repo, i.Number)
	}
	list := f.issues[repo]
	for k := range list {
		if list[k].Number == i.Number {
			list[k] = i
			return
		}
	}
	f.issues[repo] = append(list, i)
}

// Comment adds a comment to an issue.
func (f *Fake) Comment(repo string, n int, c github.Comment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fmt.Sprintf("%s#%d", repo, n)
	f.comments[k] = append(f.comments[k], c)
}

// Set changes the fake's switches under its lock.
func (f *Fake) Set(fn func(*Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// Requests is a copy of every request's "METHOD path".
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Methods...)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) gate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.Methods = append(f.Methods, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+Token {
			writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		f.mu.Lock()
		_, ok := f.issues[r.PathValue("owner")+"/"+r.PathValue("name")]
		f.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		next(w, r)
	}
}

func (f *Fake) repo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"full_name": r.PathValue("owner") + "/" + r.PathValue("name")})
}

func (f *Fake) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lists++
	n, hook := f.lists, f.OnList
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	later := n > 1 && f.RateLimitedLater > 0
	if later {
		f.RateLimitedLater--
	}
	if f.RateLimited > 0 || later {
		if !later {
			f.RateLimited--
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10))
		writeJSON(w, 403, map[string]string{"message": "API rate limit exceeded"})
		return
	}
	q := r.URL.Query()
	if q.Get("state") != "all" || q.Get("sort") != "updated" || q.Get("direction") != "asc" {
		writeJSON(w, 422, map[string]string{"message": "this fake answers state=all&sort=updated&direction=asc only"})
		return
	}
	var since time.Time
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeJSON(w, 422, map[string]string{"message": "bad since"})
			return
		}
		since = t
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	var out []github.Issue
	for _, i := range f.issues[repo] {
		if !i.UpdatedAt.Before(since) { // GitHub's since is inclusive
			out = append(out, i)
		}
	}
	slices.SortStableFunc(out, func(a, b github.Issue) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	per := f.PerPage
	lo, hi := min((page-1)*per, len(out)), min(page*per, len(out))
	if hi < len(out) {
		nq := r.URL.Query()
		nq.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.URL, r.URL.Path, nq.Encode()))
	}
	writeJSON(w, 200, append([]github.Issue{}, out[lo:hi]...))
}

func (f *Fake) listComments(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	k := r.PathValue("owner") + "/" + r.PathValue("name") + "#" + r.PathValue("n")
	out := []github.Comment{}
	for _, c := range f.comments[k] {
		if !c.UpdatedAt.Before(since) {
			out = append(out, c)
		}
	}
	// Oldest first, paginated with a Link header, as GitHub answers it
	// (this route takes no sort or direction).
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	per := f.CommentsPerPage
	lo, hi := min((page-1)*per, len(out)), min(page*per, len(out))
	if hi < len(out) {
		nq := r.URL.Query()
		nq.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.URL, r.URL.Path, nq.Encode()))
	}
	writeJSON(w, 200, append([]github.Comment{}, out[lo:hi]...))
}
