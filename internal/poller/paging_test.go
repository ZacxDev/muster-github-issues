package poller_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/github"
	"github.com/ZacxDev/muster-github-issues/internal/githubtest"
	"github.com/ZacxDev/muster-github-issues/internal/poller"
)

// The auditor's reproduction: an issue updated while a poll is between pages
// moves to the end of GitHub's updated_at-sorted list, and the issue that
// shifts across the page boundary would never be answered by page=2. Asking
// again with since= the newest update seen files all four.
func TestAnUpdateMidPollSkipsNoIssue(t *testing.T) {
	r := newRig(t)
	r.gh.Set(func(f *githubtest.Fake) {
		f.PerPage = 2
		f.OnList = func(n int) {
			if n == 2 { // after page 1 was answered: #1 is updated on GitHub
				r.gh.Put(repo, issue(1, "issue one, edited", t0.Add(10*time.Minute)))
			}
		}
	})
	for n := 1; n <= 4; n++ {
		r.gh.Put(repo, issue(n, "issue", t0.Add(time.Duration(n)*time.Minute)))
	}
	r.cycle()
	filed := map[string]bool{}
	for _, tk := range r.fake.Tasks() {
		filed[tk.Key] = true
	}
	for n := 1; n <= 4; n++ {
		if k := fmt.Sprintf("github:example-org/widgets/%d", n); !filed[k] {
			t.Errorf("issue #%d was never filed (an update mid-poll shifted it past a page boundary)", n)
		}
	}
	if len(filed) != 4 {
		t.Fatalf("filed %d of 4 issues: %v", len(filed), filed)
	}
}

// When a whole page shares one updated_at, since= cannot move on, so the poll
// follows the page link: no issue on the next page is left out, and the poll
// ends.
func TestAPageThatSharesOneUpdatedAtFollowsTheLink(t *testing.T) {
	r := newRig(t)
	r.gh.Set(func(f *githubtest.Fake) { f.PerPage = 2 })
	for n := 1; n <= 3; n++ {
		r.gh.Put(repo, issue(n, "issue", t0)) // one bulk relabel: one updated_at
	}
	r.cycle()
	if n := len(r.fake.Tasks()); n != 3 {
		t.Fatalf("filed %d of 3 issues sharing one updated_at, want 3", n)
	}
}

// The label filter decides what is FILED. Once filed, an issue's later
// changes reach its task even after the label is taken off: here the close.
func TestAFiledIssueStillGetsUpdatesAfterItsLabelIsRemoved(t *testing.T) {
	r := newRig(t, "agent")
	r.gh.Put(repo, issue(12, "Broken", t0, "agent"))
	r.cycle()
	closed := issue(12, "Broken", t0.Add(time.Hour)) // label removed, and closed
	closed.State, closed.StateReason = "closed", "completed"
	r.gh.Put(repo, closed)
	r.cycle()
	tasks := r.fake.Tasks()
	if len(tasks) != 1 || len(tasks[0].Comments) != 2 || !strings.Contains(tasks[0].Comments[1], "State: open → closed (completed)") {
		t.Fatalf("tasks = %+v, want the close on the filed task after its label was removed", tasks)
	}
	// An issue never filed is still held to the filter.
	r.gh.Put(repo, issue(13, "unlabelled", t0.Add(2*time.Hour)))
	r.cycle()
	if n := len(r.fake.Tasks()); n != 1 {
		t.Fatalf("an unlabelled issue was filed: %d tasks", n)
	}
}

// muster cuts a task body over 16 KiB from the end; the plugin puts the link
// and the metadata first and trims the issue's text itself, with a marker.
func TestALongBodyKeepsTheLinkFirstAndIsTrimmed(t *testing.T) {
	r := newRig(t)
	long := issue(12, "Huge", t0, "bug")
	long.Body = "START " + strings.Repeat("log line\n", 5000) + " END-OF-BODY"
	r.gh.Put(repo, long)
	r.cycle()
	tasks := r.fake.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d", len(tasks))
	}
	b := tasks[0].Body
	wantHead := "GitHub issue: https://github.example.com/example-org/widgets/issues/12\n" +
		"Repository: example-org/widgets, issue #12, opened by @reporter on 2026-11-18T09:00:00Z\nLabels: bug\n---\nSTART "
	if !strings.HasPrefix(b, wantHead) {
		t.Fatalf("body does not start with the link and metadata:\n%.300s", b)
	}
	if !strings.HasSuffix(b, "\n[trimmed: the full text is on GitHub]") || strings.Contains(b, "END-OF-BODY") {
		t.Fatalf("long body was not trimmed with the marker:\n…%s", b[max(0, len(b)-200):])
	}
	if len(b) > 15<<10 || len(b) < 15<<10-16 {
		t.Fatalf("body is %d bytes, want about 15 KiB", len(b))
	}
	// A short body is not trimmed.
	r.gh.Put(repo, issue(13, "Small", t0))
	r.cycle()
	if b := r.fake.Tasks()[1].Body; strings.Contains(b, "[trimmed") || !strings.HasSuffix(b, "---\nSteps to reproduce: press the button.") {
		t.Fatalf("short body = %q", b)
	}
}

// GitHub answers an issue's comments oldest first, a page at a time: "the
// last 5" must be the newest 5, read from the last page.
func TestAnUpdateQuotesTheNewestComments(t *testing.T) {
	r := newRig(t)
	r.gh.Set(func(f *githubtest.Fake) { f.CommentsPerPage = 2 })
	r.gh.Put(repo, issue(12, "Broken", t0))
	r.cycle()
	for n := 1; n <= 7; n++ {
		at := t0.Add(time.Duration(n) * time.Minute)
		r.gh.Comment(repo, 12, github.Comment{ID: int64(n), Body: fmt.Sprintf("comment number %d", n), User: github.User{Login: "reporter"}, CreatedAt: at, UpdatedAt: at})
	}
	up := issue(12, "Broken", t0.Add(10*time.Minute))
	up.Comments = 7
	r.gh.Put(repo, up)
	r.cycle()
	c := r.fake.Tasks()[0].Comments[1]
	if !strings.Contains(c, "New comments (the last 5 of 7):") {
		t.Fatalf("update does not say it quotes the last 5 of 7:\n%s", c)
	}
	for n := 1; n <= 7; n++ {
		if has := strings.Contains(c, fmt.Sprintf("comment number %d", n)); has != (n >= 3) {
			t.Errorf("comment %d quoted = %t, want %t (the newest 5):\n%s", n, has, n >= 3, c)
		}
	}
}

// The state forgets a closed issue ForgetClosedAfter past its last update, so
// it does not grow forever; an open issue, and a recently closed one, stay.
func TestClosedIssuesAreForgottenAfterTheWindow(t *testing.T) {
	r := newRig(t)
	now := t0.Add(100 * 24 * time.Hour)
	r.p.Now = func() time.Time { return now }
	old := now.Add(-poller.ForgetClosedAfter - time.Hour)
	r.p.State.Issues["github:example-org/widgets/1"] = poller.Known{State: "closed", UpdatedAt: old, Filed: true}
	r.p.State.Issues["github:example-org/widgets/2"] = poller.Known{State: "open", UpdatedAt: old, Filed: true}
	r.p.State.Issues["github:example-org/widgets/3"] = poller.Known{State: "closed", UpdatedAt: now.Add(-time.Hour), Filed: true}
	r.cycle()
	r.restart() // read back what was saved
	got := r.p.State.Issues
	if _, ok := got["github:example-org/widgets/1"]; ok {
		t.Errorf("an issue closed past the window is still in the saved state")
	}
	for _, k := range []string{"github:example-org/widgets/2", "github:example-org/widgets/3"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%s was forgotten", k)
		}
	}
}
