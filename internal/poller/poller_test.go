package poller_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/deliver"
	"github.com/ZacxDev/muster-github-issues/internal/github"
	"github.com/ZacxDev/muster-github-issues/internal/githubtest"
	"github.com/ZacxDev/muster-github-issues/internal/muster"
	"github.com/ZacxDev/muster-github-issues/internal/mustertest"
	"github.com/ZacxDev/muster-github-issues/internal/outbox"
	"github.com/ZacxDev/muster-github-issues/internal/poller"
)

const repo = "example-org/widgets"

var (
	t0    = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	repoR = github.Repo{Owner: "example-org", Name: "widgets"}
)

type rig struct {
	t      *testing.T
	dir    string
	gh     *githubtest.Fake
	fake   *mustertest.Fake
	p      *poller.Poller
	d      *deliver.Deliverer
	box    *outbox.Outbox
	logs   bytes.Buffer
	labels []string
}

func newRig(t *testing.T, labels ...string) *rig {
	r := &rig{t: t, dir: t.TempDir(), gh: githubtest.New(repo), fake: mustertest.New(), labels: labels}
	t.Cleanup(r.gh.Close)
	t.Cleanup(r.fake.Close)
	r.start()
	return r
}

// start opens the state dir: a (re)start of the plugin.
func (r *rig) start() {
	t := r.t
	box, err := outbox.Open(r.dir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { box.Close() })
	st, err := poller.OpenState(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := muster.New(r.fake.URL, mustertest.Token)
	gh := github.New(r.gh.URL, githubtest.Token)
	lg := log.New(&r.logs, "", 0)
	r.box = box
	r.p = &poller.Poller{GH: gh, Repos: []github.Repo{repoR}, Labels: r.labels, Namespace: "github", Since: t0.Add(-time.Hour), Box: box, State: st, Log: lg}
	r.d = &deliver.Deliverer{Client: c, Box: box, Log: lg, Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
}

func (r *rig) restart() { r.box.Close(); r.start() }

func (r *rig) cycle() {
	r.t.Helper()
	if err := r.p.PollAll(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	if err := r.d.Drain(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func issue(n int, title string, updated time.Time, labels ...string) github.Issue {
	i := github.Issue{Number: n, Title: title, Body: "Steps to reproduce: press the button.", State: "open",
		User: github.User{Login: "reporter"}, CreatedAt: t0, UpdatedAt: updated}
	for _, l := range labels {
		i.Labels = append(i.Labels, github.Label{Name: l})
	}
	return i
}

const key12 = "github:example-org/widgets/12"

func TestANewIssueIsFiledOnceWithTitleBodyLinkAndLabels(t *testing.T) {
	r := newRig(t)
	r.gh.Put(repo, issue(12, "The button  does nothing", t0, "bug", "agent"))
	r.cycle()
	tasks := r.fake.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}
	tk := tasks[0]
	if tk.Key != key12 || tk.Title != "example-org/widgets#12: The button does nothing" {
		t.Fatalf("key %q title %q", tk.Key, tk.Title)
	}
	for _, want := range []string{"Steps to reproduce: press the button.", "GitHub issue: https://github.example.com/example-org/widgets/issues/12",
		"opened by @reporter on 2026-11-18T09:00:00Z", "Labels: bug, agent"} {
		if !strings.Contains(tk.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, tk.Body)
		}
	}
	wantComment := "[" + key12 + "@2026-11-18T09:00:00Z] new GitHub issue example-org/widgets#12 by @reporter: https://github.example.com/example-org/widgets/issues/12"
	if len(tk.Comments) != 1 || tk.Comments[0] != wantComment {
		t.Fatalf("comments = %q\nwant %q", tk.Comments, wantComment)
	}
	req := r.fake.Reqs()[0]
	if !strings.Contains(req, `"route_to_chief":true`) || !strings.Contains(req, `"notice":"assigned"`) {
		t.Fatalf("not routed as assigned: %s", req)
	}
}

// The dedupe: polling again, and polling after a restart, with no change on
// GitHub (whose since is inclusive, so the same issue is answered again),
// sends muster nothing more.
func TestAnUnchangedIssueIsNeverFiledTwiceAcrossPollsAndRestarts(t *testing.T) {
	r := newRig(t)
	i := issue(12, "Broken", t0)
	i.Comments = 1
	r.gh.Put(repo, i)
	r.cycle()
	r.cycle()
	r.restart()
	r.cycle()
	if n := r.fake.RequestCount(); n != 1 {
		t.Fatalf("muster got %d requests, want 1", n)
	}
	// Nor is GitHub asked for the comments of an issue that did not change.
	for _, q := range r.gh.Requests() {
		if strings.HasSuffix(q, "/comments") {
			t.Fatalf("an unchanged issue's comments were fetched: %s", q)
		}
	}
	if n := len(r.fake.Tasks()); n != 1 {
		t.Fatalf("tasks = %d, want 1", n)
	}
}

// Restart mid-batch, the worst case: the events of a poll were recorded in
// the outbox but the process died before the cursor and the issue states were
// saved, and before any delivery. The next start polls the same issues again:
// every event is the same id, so none is recorded twice, and each is filed
// once.
func TestARestartBeforeTheStateIsSavedFilesNothingTwice(t *testing.T) {
	r := newRig(t)
	r.cycle() // the first poll saves the starting cursor
	before, err := os.ReadFile(filepath.Join(r.dir, poller.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		r.gh.Put(repo, issue(n, "issue", t0.Add(time.Duration(n)*time.Minute)))
	}
	if err := r.p.PollAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.box.Len() != 3 {
		t.Fatalf("pending = %d, want 3", r.box.Len())
	}
	// The "crash": the state file as it was before the poll.
	os.WriteFile(filepath.Join(r.dir, poller.StateFile), before, 0o600)
	r.restart()
	r.cycle()
	if n := r.fake.RequestCount(); n != 3 {
		t.Fatalf("muster got %d requests, want 3", n)
	}
	for _, tk := range r.fake.Tasks() {
		if len(tk.Comments) != 1 {
			t.Fatalf("task %s has %d comments, want 1", tk.Key, len(tk.Comments))
		}
	}
}

// A poll cut off mid-way (a rate limit on page 2) keeps page 1 and its
// cursor; the next poll takes up from there and skips nothing.
func TestAPollCutOffMidWaySkipsNothing(t *testing.T) {
	r := newRig(t)
	r.gh.Set(func(f *githubtest.Fake) { f.PerPage = 2; f.RateLimitedLater = 1 })
	for n := 1; n <= 5; n++ {
		r.gh.Put(repo, issue(n, "issue", t0.Add(time.Duration(n)*time.Minute)))
	}
	err := r.p.PollAll(context.Background())
	if e := github.AsError(err); e == nil || e.RetryAt.IsZero() {
		t.Fatalf("poll = %v, want a rate limit with a retry time", err)
	}
	if r.box.Len() != 2 {
		t.Fatalf("recorded = %d, want page 1's 2", r.box.Len())
	}
	r.cycle()
	if n := len(r.fake.Tasks()); n != 5 {
		t.Fatalf("tasks = %d, want 5", n)
	}
	if n := r.fake.RequestCount(); n != 5 {
		t.Fatalf("requests = %d, want 5", n)
	}
}

func TestAnUpdateIsACommentOnTheSameTaskWithTheNewComments(t *testing.T) {
	r := newRig(t)
	r.gh.Put(repo, issue(12, "Broken", t0))
	r.cycle()
	t1 := t0.Add(30 * time.Minute)
	r.gh.Comment(repo, 12, github.Comment{ID: 1, Body: "Still broken on 2.3.", User: github.User{Login: "reporter"}, CreatedAt: t1, UpdatedAt: t1})
	up := issue(12, "Broken", t1, "bug")
	up.Comments = 1
	r.gh.Put(repo, up)
	r.cycle()
	tasks := r.fake.Tasks()
	if len(tasks) != 1 || len(tasks[0].Comments) != 2 {
		t.Fatalf("tasks = %+v, want one with two comments", tasks)
	}
	want := "[" + key12 + "@2026-11-18T09:30:00Z] updated on GitHub: https://github.example.com/example-org/widgets/issues/12\n" +
		"State: open\nLabels: bug\nNew comments:\n\n@reporter at 2026-11-18T09:30:00Z:\nStill broken on 2.3."
	if got := tasks[0].Comments[1]; got != want {
		t.Fatalf("update comment =\n%s\nwant\n%s", got, want)
	}
	req := r.fake.Reqs()[1]
	if !strings.Contains(req, `"notice":"update"`) || strings.Contains(req, `"create"`) {
		t.Fatalf("the update should be a routed comment with no create: %s", req)
	}
}

func TestCloseAndReopen(t *testing.T) {
	r := newRig(t)
	r.gh.Put(repo, issue(12, "Broken", t0))
	r.cycle()
	closed := issue(12, "Broken", t0.Add(time.Hour))
	closed.State, closed.StateReason = "closed", "completed"
	r.gh.Put(repo, closed)
	r.cycle()
	tasks := r.fake.Tasks()
	if len(tasks) != 1 || !strings.Contains(tasks[0].Comments[1], "State: open → closed (completed)") {
		t.Fatalf("close = %+v", tasks)
	}
	r.fake.Complete(key12) // the owner finished it in muster
	r.gh.Put(repo, issue(12, "Broken", t0.Add(2*time.Hour)))
	r.cycle()
	tasks = r.fake.Tasks()
	if len(tasks) != 2 || !tasks[1].Open || !strings.Contains(tasks[1].Comments[0], "reopened on GitHub") {
		t.Fatalf("reopen = %+v, want a second, open task", tasks)
	}
}

func TestALabelFilterFilesOnlyMatchingIssuesAndAPullRequestIsNever(t *testing.T) {
	r := newRig(t, "agent", "triage")
	r.gh.Put(repo, issue(1, "unlabelled", t0))
	r.gh.Put(repo, issue(2, "labelled", t0, "Agent"))
	pr := issue(3, "a pull request", t0, "agent")
	raw := json.RawMessage(`{"url":"x"}`)
	pr.PullRequest = &raw
	r.gh.Put(repo, pr)
	closed := issue(4, "closed already", t0, "agent")
	closed.State = "closed"
	r.gh.Put(repo, closed)
	r.cycle()
	tasks := r.fake.Tasks()
	if len(tasks) != 1 || tasks[0].Key != "github:example-org/widgets/2" {
		t.Fatalf("tasks = %+v, want only #2", tasks)
	}
	// #1 gains the label: it is filed then, as new.
	r.gh.Put(repo, issue(1, "unlabelled", t0.Add(time.Minute), "triage"))
	r.cycle()
	if tasks = r.fake.Tasks(); len(tasks) != 2 || !strings.Contains(tasks[1].Comments[0], "new GitHub issue") {
		t.Fatalf("after labelling = %+v", tasks)
	}
}

// Every request this plugin sends is a COPY task: muster refuses event_id
// there (400 "event_id is only for reference"), and so does the fake.
func TestNoRequestCarriesEventID(t *testing.T) {
	r := newRig(t)
	r.gh.Put(repo, issue(12, "Broken", t0))
	r.cycle()
	r.gh.Put(repo, issue(12, "Broken", t0.Add(time.Minute), "bug"))
	r.cycle()
	reqs := r.fake.Reqs()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	for _, q := range reqs {
		if strings.Contains(q, "event_id") || strings.Contains(q, "reference") {
			t.Fatalf("a copy-task request carries a reference field: %s", q)
		}
	}
	if tasks := r.fake.Tasks(); len(tasks) != 1 || len(tasks[0].Comments) != 2 {
		t.Fatalf("tasks = %+v", tasks)
	}
}

// A retry after muster rate limited the request, and the resend after a
// restart, are byte for byte the first request: muster's comment dedupe and
// its routed-event id both depend on the comment text.
func TestARetryIsByteIdentical(t *testing.T) {
	r := newRig(t)
	r.gh.Put(repo, issue(12, "Broken", t0))
	if err := r.p.PollAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.fake.Set(func(f *mustertest.Fake) { f.RateLimit = 1 })
	// One attempt, then a restart before the retry lands.
	ctx, cancel := context.WithCancel(context.Background())
	r.d.Sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	_ = r.d.Drain(ctx)
	r.restart()
	r.gh.Put(repo, issue(12, "Broken — edited title", t0)) // GitHub answers differently now: the stored bytes still win
	r.cycle()
	reqs := r.fake.Reqs()
	if len(reqs) != 2 || reqs[0] != reqs[1] {
		t.Fatalf("requests =\n%s\nwant two identical", strings.Join(reqs, "\n"))
	}
}

func TestRunStopsOnA401AndWaitsOutARateLimit(t *testing.T) {
	r := newRig(t)
	r.gh.Set(func(f *githubtest.Fake) { f.RateLimited = 1 })
	var waits []time.Duration
	calls := 0
	sleep := func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if calls++; calls == 2 {
			return context.Canceled
		}
		return nil
	}
	_ = r.p.Run(context.Background(), 30*time.Second, sleep)
	if len(waits) != 2 || waits[0] < 60*time.Second || waits[0] > 91*time.Second || waits[1] != 30*time.Second {
		t.Fatalf("waits = %v, want the rate-limit reset (~90s), then the interval", waits)
	}

	r.p.GH = github.New(r.gh.URL, "wrong-token")
	err := r.p.Run(context.Background(), time.Second, func(context.Context, time.Duration) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "GitHub refused GITHUB_TOKEN") {
		t.Fatalf("run with a bad token = %v", err)
	}
}

func TestOnlyGETRequestsReachGitHub(t *testing.T) {
	r := newRig(t)
	r.gh.Put(repo, issue(12, "Broken", t0))
	r.cycle()
	for _, m := range r.gh.Requests() {
		if !strings.HasPrefix(m, "GET ") {
			t.Fatalf("a %s reached GitHub", m)
		}
	}
}

func TestALongRepositoryNameIsHashedIntoAValidKey(t *testing.T) {
	long := github.Repo{Owner: "an-organisation-with-a-long-name", Name: "and-a-repository-name-that-is-longer-still"}
	k := poller.Key("github", long, 4711)
	if len(k) > 64 || !strings.HasPrefix(k, "github:") || !strings.HasSuffix(k, "/4711") || strings.ContainsAny(k, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Fatalf("key = %q", k)
	}
	if got := poller.Key("github", github.Repo{Owner: "Example-Org", Name: "Widgets"}, 1); got != "github:example-org/widgets/1" {
		t.Fatalf("key = %q", got)
	}
}
