// Package poller turns GitHub issues into muster task events. Each poll asks
// every configured repository for the issues updated since its cursor, and
// records one event per issue change in the outbox:
//
//   - an open issue the plugin has not filed yet (new, or newly carrying a
//     filter label): a task, routed to the chief as "assigned";
//   - an issue reopened after the plugin saw it closed: the same, so a task
//     completed meanwhile is filed again;
//   - any other change to a filed issue (an edit, a comment, a label, a
//     close): a comment on its task, routed as an "update" (muster passes it
//     on only while an agent works the task; otherwise it is filed only).
//
// The event id is the task key plus the issue's updated_at, and every
// comment starts with it in brackets: muster dedupes a comment by its text
// and names the routed event by it, and the outbox refuses an id it holds.
package poller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster-github-issues/internal/github"
	"github.com/ZacxDev/muster-github-issues/internal/muster"
	"github.com/ZacxDev/muster-github-issues/internal/outbox"
)

// MaxNewComments bounds how many new GitHub comments one update quotes, and
// MaxCommentBytes how much of each.
const (
	MaxNewComments  = 5
	MaxCommentBytes = 2000
)

// Poller is one polling loop. Every field but Labels, Log and Now is required.
type Poller struct {
	GH        *github.Client
	Repos     []github.Repo
	Labels    []string // an issue must carry at least one (none: every issue)
	Namespace string
	Since     time.Time // where a repository with no cursor yet starts
	Box       *outbox.Outbox
	State     *State
	Log       *log.Logger
	Now       func() time.Time // nil: time.Now
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Key is the muster task key for an issue: <namespace>:<owner>/<name>/<n>,
// lower-case (muster's tag grammar). A repository name too long for muster's
// 64-character tag is replaced by 12 hex characters of its sha256.
func Key(namespace string, r github.Repo, number int) string {
	repo := strings.ToLower(r.String())
	k := namespace + ":" + repo + "/" + strconv.Itoa(number)
	if utf8.RuneCountInString(k) <= 64 {
		return k
	}
	sum := sha256.Sum256([]byte(repo))
	return namespace + ":" + hex.EncodeToString(sum[:6]) + "/" + strconv.Itoa(number)
}

// EventID names one change of one issue.
func EventID(key string, updated time.Time) string {
	return key + "@" + updated.UTC().Format(time.RFC3339)
}

func (p *Poller) matches(i github.Issue) bool {
	if len(p.Labels) == 0 {
		return true
	}
	for _, l := range i.Labels {
		for _, want := range p.Labels {
			if strings.EqualFold(l.Name, want) {
				return true
			}
		}
	}
	return false
}

// PollAll polls every repository once. It stops at the first error that
// should stop the loop (a 401, a cancelled ctx) and logs the others. First it
// forgets the closed issues older than ForgetClosedAfter.
func (p *Poller) PollAll(ctx context.Context) error {
	if n := p.State.ForgetClosed(p.now(), ForgetClosedAfter); n > 0 {
		if err := p.State.Save(); err != nil {
			return err
		}
		p.Log.Printf("forgot %d issue(s) closed more than %s ago", n, ForgetClosedAfter)
	}
	for _, r := range p.Repos {
		if err := p.PollRepo(ctx, r); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if e := github.AsError(err); e != nil && (e.Unauthorized() || !e.RetryAt.IsZero()) {
				return err
			}
			p.Log.Printf("poll %s: %v; trying again next poll", r, err)
		}
	}
	return nil
}

// Run polls every interval until ctx ends or GitHub refuses the token. A rate
// limit waits until GitHub's reset time (at most an hour) before polling again.
func (p *Poller) Run(ctx context.Context, interval time.Duration, sleep func(context.Context, time.Duration) error) error {
	for {
		wait := interval
		if err := p.PollAll(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e := github.AsError(err)
			switch {
			case e != nil && e.Unauthorized():
				return fmt.Errorf("stopping: GitHub refused GITHUB_TOKEN (expired or revoked?): %v", err)
			case e != nil && !e.RetryAt.IsZero():
				wait = min(max(time.Until(e.RetryAt), interval), time.Hour)
				p.Log.Printf("GitHub rate limit (%v); polling again in %s", err, wait.Round(time.Second))
			}
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// PollRepo records every change to r's issues since its cursor and moves the
// cursor on after each page. An error leaves the cursor at the last page fully
// recorded, so nothing is skipped.
//
// It does NOT walk GitHub's page=2, page=3 … links: those are offsets into a
// list sorted by updated_at, so an issue updated while the poll is between
// pages moves to the end of the list, every issue after it shifts one place
// up, and the one that shifts across the page boundary is never answered. It
// asks again instead, with since= the newest updated_at it has recorded: an
// update mid-poll then only puts the issue further on, where a later ask finds
// it. Only when a whole page shares one updated_at (so since= cannot move on)
// does it follow the page link.
func (p *Poller) PollRepo(ctx context.Context, r github.Repo) error {
	cursor, ok := p.State.Cursors[r.String()]
	if !ok {
		cursor = p.Since.UTC().Truncate(time.Second)
		p.State.Cursors[r.String()] = cursor
		if err := p.State.Save(); err != nil {
			return err
		}
	}
	page := ""
	for {
		issues, next, err := p.GH.IssuesPage(ctx, r, cursor, page)
		if err != nil {
			return err
		}
		newest := cursor
		for _, iss := range issues {
			if iss.UpdatedAt.After(newest) {
				newest = iss.UpdatedAt
			}
			if iss.PullRequest != nil {
				continue
			}
			if err := p.record(ctx, r, iss); err != nil {
				return err
			}
		}
		p.State.Cursors[r.String()] = newest
		if err := p.State.Save(); err != nil {
			return err
		}
		switch {
		case next == "":
			return nil
		case newest.After(cursor):
			cursor, page = newest, "" // ask again from the newest update seen
		default:
			page = next // the whole page shares one updated_at: since= cannot move on
		}
	}
}

// record turns one issue, as GitHub answers it now, into at most one event.
// The label filter decides whether an issue is FILED; once it is, every change
// to it reaches its task, a removed label included.
func (p *Poller) record(ctx context.Context, r github.Repo, iss github.Issue) error {
	key := Key(p.Namespace, r, iss.Number)
	id := EventID(key, iss.UpdatedAt)
	known, seen := p.State.Issues[key]
	if seen && !iss.UpdatedAt.After(known.UpdatedAt) {
		return nil // already recorded (GitHub's since is inclusive)
	}
	if !known.Filed && !p.matches(iss) {
		return nil
	}
	var task muster.Task
	switch {
	case !known.Filed && iss.State != "open":
		// Seen closed before any task was filed: nothing to do, but
		// remember it, so a reopen is filed.
		p.State.Issues[key] = Known{State: iss.State, UpdatedAt: iss.UpdatedAt}
		return nil
	case !known.Filed:
		// Never filed: new, or first seen closed and now reopened. Either
		// way muster has no task for it yet, so it is a new issue to it.
		task = p.newTask(r, key, id, iss, fmt.Sprintf("new GitHub issue %s#%d by @%s: %s", r, iss.Number, iss.User.Login, iss.HTMLURL))
	case known.State != "open" && iss.State == "open":
		task = p.newTask(r, key, id, iss, "reopened on GitHub: "+iss.HTMLURL)
	default:
		var fresh []github.Comment
		total := 0
		if iss.Comments > 0 {
			var err error
			fresh, total, err = p.GH.Comments(ctx, r, iss.Number, known.UpdatedAt.Add(time.Second), MaxNewComments)
			if err != nil {
				return err
			}
		}
		task = muster.Task{Key: key, Comment: updateComment(id, known, iss, fresh, total), RouteToChief: true, Notice: muster.NoticeUpdate}
	}
	added, err := p.Box.Add(id, task)
	if err != nil {
		return err
	}
	if added {
		p.Log.Printf("event %s: recorded", id)
	}
	p.State.Issues[key] = Known{State: iss.State, UpdatedAt: iss.UpdatedAt, Filed: true}
	return nil
}

func (p *Poller) newTask(r github.Repo, key, id string, iss github.Issue, line string) muster.Task {
	return muster.Task{
		Key:          key,
		Create:       &muster.TaskCreate{Title: fmt.Sprintf("%s#%d: %s", r, iss.Number, oneLine(iss.Title)), Body: issueBody(r, iss)},
		Comment:      "[" + id + "] " + line,
		RouteToChief: true,
		Notice:       muster.NoticeAssigned,
	}
}

// MaxBodyBytes bounds a task body. muster cuts a longer one at 16 KiB from
// the END, which would drop whatever came last, so the plugin puts the link
// and the metadata first and trims the issue's own text itself.
const MaxBodyBytes = 15 << 10

// TrimmedMarker ends an issue body the plugin trimmed.
const TrimmedMarker = "\n[trimmed: the full text is on GitHub]"

func issueBody(r github.Repo, iss github.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub issue: %s\nRepository: %s, issue #%d, opened by @%s on %s\n",
		iss.HTMLURL, r, iss.Number, iss.User.Login, iss.CreatedAt.UTC().Format(time.RFC3339))
	if names := iss.LabelNames(); len(names) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(names, ", "))
	}
	b.WriteString("---\n")
	body := strings.TrimSpace(iss.Body)
	if body == "" {
		body = "(no description)"
	}
	if room := MaxBodyBytes - b.Len() - len(TrimmedMarker); len(body) > room {
		body = cutBytes(body, room) + TrimmedMarker
	}
	b.WriteString(body)
	return b.String()
}

func updateComment(id string, known Known, iss github.Issue, fresh []github.Comment, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] updated on GitHub: %s\n", id, iss.HTMLURL)
	state := iss.State
	if iss.StateReason != "" {
		state += " (" + iss.StateReason + ")"
	}
	if known.State != iss.State {
		fmt.Fprintf(&b, "State: %s → %s\n", known.State, state)
	} else {
		fmt.Fprintf(&b, "State: %s\n", state)
	}
	names := iss.LabelNames()
	slices.Sort(names)
	if len(names) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(names, ", "))
	}
	if len(fresh) > 0 {
		shown := fresh
		if len(shown) > MaxNewComments {
			shown = shown[len(shown)-MaxNewComments:]
		}
		if total > len(shown) {
			fmt.Fprintf(&b, "New comments (the last %d of %d):\n", len(shown), total)
		} else {
			b.WriteString("New comments:\n")
		}
		for _, c := range shown {
			fmt.Fprintf(&b, "\n@%s at %s:\n%s\n", c.User.Login, c.CreatedAt.UTC().Format(time.RFC3339), cut(strings.TrimSpace(c.Body), MaxCommentBytes))
		}
	}
	return b.String()
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return cutBytes(s, n) + " …"
}

// cutBytes is s cut to at most n bytes on a character boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:max(n, 0)]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
