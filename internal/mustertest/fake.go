// Package mustertest is a fake muster for tests: GET /plugin/v1/self,
// GET /plugin/v1/tasks (the namespace check) and POST /plugin/v1/tasks,
// answering the way muster does (docs/plugins.md, "POST /plugin/v1/tasks"):
// one open task per key, a comment added once, the same validation, and
// switches for the failures a plugin must survive.
package mustertest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Token is the plugin token the fake accepts.
const Token = "mpl_test_token_0123456789"

// Namespaces are the plugin task namespaces the fake accepts
// (MUSTER_PLUGIN_TASK_NAMESPACES on a real server).
var Namespaces = []string{"github", "alert"}

// Task is one task the fake holds.
type Task struct {
	ID       int64
	Key      string
	Title    string
	Body     string
	Comments []string
	Open     bool
}

// Fake is a fake muster server.
type Fake struct {
	*httptest.Server

	mu sync.Mutex
	// Requests are the raw bodies of every POST /plugin/v1/tasks, in order.
	Requests []string
	// RateLimit answers the next n task posts 429 with Retry-After: 7.
	RateLimit int
	// Unavailable answers the next n task posts 503 with Retry-After: 3,
	// storing nothing.
	Unavailable int
	// Conflict files the task but answers the next n task posts that route
	// 409, as muster does when the event's external_id is already taken.
	Conflict int
	// Down drops every connection without an answer, as a muster that is
	// not there does.
	Down bool
	// Revoked makes every route answer 401.
	Revoked bool
	// Off makes every route answer 404, as muster does with plugins off.
	Off bool

	tasks []*Task
}

// New starts a fake muster; Close it when done.
func New() *Fake {
	f := &Fake{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /plugin/v1/self", f.gate(func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, `{"plugin_id":3,"plugin_name":"example","owner_id":1,"owner_login":"operator","owner_kind":"person","capabilities":["messages"],"deliver_to":null}`)
	}))
	mux.HandleFunc("POST /plugin/v1/tasks", f.gate(f.task))
	mux.HandleFunc("GET /plugin/v1/tasks", f.gate(f.list))
	f.Server = httptest.NewServer(mux)
	return f
}

// Set changes the fake's switches under its lock.
func (f *Fake) Set(fn func(f *Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// Tasks returns copies of every task, oldest first.
func (f *Fake) Tasks() []Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Task, 0, len(f.tasks))
	for _, t := range f.tasks {
		c := *t
		c.Comments = append([]string(nil), t.Comments...)
		out = append(out, c)
	}
	return out
}

// Reqs returns a copy of the raw task request bodies.
func (f *Fake) Reqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Requests...)
}

// RequestCount is how many task posts the fake received.
func (f *Fake) RequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}

// Complete closes the open task for key, as the owner completing it would.
func (f *Fake) Complete(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.open(key); t != nil {
		t.Open = false
	}
}

func write(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func (f *Fake) gate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		off, revoked, down := f.Off, f.Revoked, f.Down
		f.mu.Unlock()
		if down {
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}
		if off {
			http.NotFound(w, r)
			return
		}
		if revoked || r.Header.Get("Authorization") != "Bearer "+Token {
			write(w, 401, `{"error":"this route takes a plugin token (mpl_…), minted at /plugins"}`)
			return
		}
		next(w, r)
	}
}

var tagRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}:[a-z0-9._/-]+$`)

type taskIn struct {
	Key    string `json:"key"`
	Create *struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"create"`
	Comment      string          `json:"comment"`
	RouteToChief bool            `json:"route_to_chief"`
	Notice       string          `json:"notice"`
	Reference    json.RawMessage `json:"reference"`
	EventID      string          `json:"event_id"`
}

// refusal is muster's validatePluginTask for a copy task, or "".
func refusal(in taskIn) string {
	ns, _, _ := strings.Cut(in.Key, ":")
	nsOK := false
	for _, n := range Namespaces {
		nsOK = nsOK || n == ns
	}
	switch {
	case !tagRE.MatchString(in.Key) || len([]rune(in.Key)) > 64:
		return "key is required and must be a normalized tag (lower-case namespace:value)"
	case !nsOK:
		return "key must be <namespace>:<id> in one of this server's plugin task namespaces"
	case in.Reference != nil:
		return "this fake files copy tasks only"
	case in.EventID != "":
		return "event_id is only for reference"
	case in.Create == nil && in.Comment == "":
		return "nothing to do: give create, a non-blank comment, or both"
	case in.RouteToChief && in.Comment == "":
		return "route_to_chief needs a non-blank comment: it names the event"
	case in.RouteToChief && in.Notice != "assigned" && in.Notice != "mentioned" && in.Notice != "update":
		return "route_to_chief needs notice: assigned, mentioned or update"
	case !in.RouteToChief && in.Notice != "":
		return "notice is only for route_to_chief"
	case in.Create != nil && strings.TrimSpace(in.Create.Body) == "":
		return "create.body is required"
	}
	return ""
}

// list is GET /plugin/v1/tasks?namespace=<ns>&live=true, as muster answers
// it (internal/api/plugin_task_refs.go, handlePluginTaskList): 400 for a
// namespace not in the server's list or without live=true. The fake lists no
// rows: this plugin asks only for the status.
func (f *Fake) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ok := false
	for _, n := range Namespaces {
		ok = ok || n == q.Get("namespace")
	}
	switch {
	case !ok:
		b, _ := json.Marshal(map[string]string{"error": "namespace must be one of this server's plugin task namespaces (" + strings.Join(Namespaces, ", ") + ")"})
		write(w, 400, string(b))
	case q.Get("live") != "true":
		write(w, 400, `{"error":"live=true is required (only live rows are listed)"}`)
	default:
		write(w, 200, `{"tasks":[],"truncated":false,"next_after":null}`)
	}
}

func (f *Fake) open(key string) *Task {
	for _, t := range f.tasks {
		if t.Key == key && t.Open {
			return t
		}
	}
	return nil
}

func (f *Fake) task(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, string(raw))
	if f.RateLimit > 0 {
		f.RateLimit--
		w.Header().Set("Retry-After", "7")
		write(w, 429, `{"error":"this plugin's task-event caps are full"}`)
		return
	}
	if f.Unavailable > 0 {
		f.Unavailable--
		w.Header().Set("Retry-After", "3")
		write(w, 503, `{"error":"this server files no plugin tasks: MUSTER_PLUGIN_TASK_NAMESPACES is empty"}`)
		return
	}
	var in taskIn
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		write(w, 400, `{"error":"the body is {\"key\", \"create\": {\"title\", \"body\"}, \"comment\"}"}`)
		return
	}
	in.Comment = strings.TrimSpace(in.Comment)
	if msg := refusal(in); msg != "" {
		b, _ := json.Marshal(map[string]string{"error": msg})
		write(w, 400, string(b))
		return
	}
	t := f.open(in.Key)
	created, commented := false, false
	switch {
	case t == nil && in.Create == nil:
		write(w, 200, `{"id":null,"created":false,"commented":false}`+"\n")
		return
	case t == nil:
		t = &Task{ID: int64(len(f.tasks) + 1), Key: in.Key, Title: in.Create.Title, Body: in.Create.Body, Open: true}
		f.tasks = append(f.tasks, t)
		created = true
	}
	if in.Comment != "" {
		dup := false
		for _, c := range t.Comments {
			dup = dup || c == in.Comment
		}
		if !dup {
			t.Comments = append(t.Comments, in.Comment)
			commented = true
		}
	}
	out := map[string]any{"id": t.ID, "created": created, "commented": commented}
	if in.RouteToChief {
		if f.Conflict > 0 {
			f.Conflict--
			write(w, 409, `{"error":"the task is filed, but this plugin already holds a message under the task event's external_id","id":`+strconv.FormatInt(t.ID, 10)+`}`)
			return
		}
		var routed any = false
		if in.Notice == "assigned" {
			routed = "chief"
		}
		out["routed"] = routed
	}
	code := 200
	if created {
		code = 201
	}
	b, _ := json.Marshal(out)
	write(w, code, string(b))
}
