package deliver_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/deliver"
	"github.com/ZacxDev/muster-github-issues/internal/muster"
	"github.com/ZacxDev/muster-github-issues/internal/mustertest"
	"github.com/ZacxDev/muster-github-issues/internal/outbox"
)

type rig struct {
	fake   *mustertest.Fake
	box    *outbox.Outbox
	d      *deliver.Deliverer
	logs   *bytes.Buffer
	sleeps []time.Duration
}

func newRig(t *testing.T, dir string) *rig {
	t.Helper()
	r := &rig{fake: mustertest.New(), logs: &bytes.Buffer{}}
	t.Cleanup(r.fake.Close)
	r.open(t, dir)
	return r
}

func (r *rig) open(t *testing.T, dir string) {
	t.Helper()
	box, err := outbox.Open(dir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { box.Close() })
	c, err := muster.New(r.fake.URL, mustertest.Token)
	if err != nil {
		t.Fatal(err)
	}
	r.box = box
	r.d = &deliver.Deliverer{Client: c, Box: box, Log: log.New(r.logs, "", 0),
		Sleep: func(ctx context.Context, d time.Duration) error { r.sleeps = append(r.sleeps, d); return ctx.Err() }}
}

func newTask(key, ev string) muster.Task {
	return muster.Task{Key: key, Create: &muster.TaskCreate{Title: "T " + key, Body: "body"},
		Comment: "[" + ev + "] new", RouteToChief: true, Notice: muster.NoticeAssigned}
}

func (r *rig) add(t *testing.T, id string, task muster.Task) {
	t.Helper()
	if ok, err := r.box.Add(id, task); !ok || err != nil {
		t.Fatalf("add %s = %t, %v", id, ok, err)
	}
}

func TestAnEventIsFiledOnceAndMarkedDone(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.add(t, "e1", newTask("alert:a", "e1"))
	if err := r.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	tasks := r.fake.Tasks()
	if len(tasks) != 1 || tasks[0].Key != "alert:a" || len(tasks[0].Comments) != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if r.box.Len() != 0 {
		t.Fatal("the event is still pending after a 201")
	}
	if !strings.Contains(r.logs.String(), "event e1: task 1 (created true, commented true, routed chief)") {
		t.Fatalf("log = %q", r.logs.String())
	}
}

// muster down: no answer at all. The same request is sent again after a
// doubling backoff, and the event stays pending until it lands.
func TestMusterDownIsRetriedWithBackoffThenDelivered(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.add(t, "e1", newTask("alert:a", "e1"))
	r.fake.Set(func(f *mustertest.Fake) { f.Down = true })
	calls := 0
	r.d.Sleep = func(ctx context.Context, d time.Duration) error {
		r.sleeps = append(r.sleeps, d)
		if calls++; calls == 3 {
			r.fake.Set(func(f *mustertest.Fake) { f.Down = false })
		}
		return nil
	}
	if err := r.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.sleeps; len(got) != 3 || got[0] != 5*time.Second || got[1] != 10*time.Second || got[2] != 20*time.Second {
		t.Fatalf("waits = %v, want 5s 10s 20s", got)
	}
	if n := len(r.fake.Tasks()); n != 1 {
		t.Fatalf("tasks = %d, want 1", n)
	}
}

// A context that ends while muster is down leaves the event pending: it is
// not dropped, and the next start sends it.
func TestAStopWhileMusterIsDownKeepsTheEvent(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.add(t, "e1", newTask("alert:a", "e1"))
	r.fake.Set(func(f *mustertest.Fake) { f.Down = true })
	ctx, cancel := context.WithCancel(context.Background())
	r.d.Sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	if err := r.d.Drain(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("drain = %v", err)
	}
	if r.box.Len() != 1 {
		t.Fatal("the event was dropped while muster was down")
	}
}

func TestA429WaitsRetryAfterAndSendsTheSameRequest(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.add(t, "e1", newTask("alert:a", "e1"))
	r.fake.Set(func(f *mustertest.Fake) { f.RateLimit = 2 })
	if err := r.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.sleeps) != 2 || r.sleeps[0] != 7*time.Second || r.sleeps[1] != 7*time.Second {
		t.Fatalf("waits = %v, want 7s 7s (Retry-After)", r.sleeps)
	}
	reqs := r.fake.Reqs()
	if len(reqs) != 3 || reqs[0] != reqs[2] {
		t.Fatalf("requests = %d, the retry not identical to the first", len(reqs))
	}
	if n := len(r.fake.Tasks()); n != 1 {
		t.Fatalf("tasks = %d, want 1", n)
	}
}

func TestA503HonoursRetryAfterAndIsSentAgain(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.add(t, "e1", newTask("alert:a", "e1"))
	r.fake.Set(func(f *mustertest.Fake) { f.Unavailable = 1 })
	r.d.Backoff = time.Second
	if err := r.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.sleeps) != 1 || r.sleeps[0] != 3*time.Second {
		t.Fatalf("waits = %v, want 3s (Retry-After over the 1s backoff)", r.sleeps)
	}
	if n := len(r.fake.Tasks()); n != 1 {
		t.Fatalf("tasks = %d, want 1", n)
	}
}

// 409: muster filed the task but will never route the event. Resending cannot
// help, so it is not sent again, and the queue moves on.
func TestA409IsNotSentAgainAndTheQueueMovesOn(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.add(t, "e1", newTask("alert:a", "e1"))
	r.add(t, "e2", newTask("alert:b", "e2"))
	r.fake.Set(func(f *mustertest.Fake) { f.Conflict = 1 })
	if err := r.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := r.fake.RequestCount(); n != 2 {
		t.Fatalf("requests = %d, want 2 (the 409 not resent)", n)
	}
	if len(r.sleeps) != 0 || r.box.Len() != 0 {
		t.Fatalf("waits %v, pending %d", r.sleeps, r.box.Len())
	}
	if !strings.Contains(r.logs.String(), "event e1: muster filed the task but will not route this event") {
		t.Fatalf("log = %q", r.logs.String())
	}
}

func TestA400IsDroppedNotRetried(t *testing.T) {
	r := newRig(t, t.TempDir())
	r.add(t, "e1", muster.Task{Key: "nope:x", Comment: "[e1]"})
	if err := r.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.fake.RequestCount() != 1 || r.box.Len() != 0 || !strings.Contains(r.logs.String(), "refused for good, dropped") {
		t.Fatalf("requests %d, pending %d, log %q", r.fake.RequestCount(), r.box.Len(), r.logs.String())
	}
}

func TestARevokedTokenOrPluginsOffStopsAndKeepsTheEvent(t *testing.T) {
	for name, set := range map[string]func(*mustertest.Fake){
		"revoked": func(f *mustertest.Fake) { f.Revoked = true },
		"off":     func(f *mustertest.Fake) { f.Off = true },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, t.TempDir())
			r.add(t, "e1", newTask("alert:a", "e1"))
			r.fake.Set(set)
			if err := r.d.Drain(context.Background()); !errors.Is(err, deliver.ErrStop) {
				t.Fatalf("drain = %v, want ErrStop", err)
			}
			if r.box.Len() != 1 {
				t.Fatal("the event was dropped on a stop")
			}
		})
	}
}

// Restart mid-batch, the worst case: muster stored the first event but the
// process died before marking it done. The next start sends it again; muster
// finds the task and the comment and adds neither, and the rest of the batch
// follows. One task per key, one comment per event.
func TestARestartMidBatchFilesNothingTwice(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.add(t, "e1", newTask("alert:a", "e1"))
	r.add(t, "e2", newTask("alert:b", "e2"))
	// "Crash": deliver e1 to muster without marking it done.
	if _, err := r.d.Client.PostTask(context.Background(), r.box.Pending()[0].Task); err != nil {
		t.Fatal(err)
	}
	r.box.Close()

	r.open(t, dir) // the restart: same muster, same state dir
	if n := r.box.Len(); n != 2 {
		t.Fatalf("pending after restart = %d, want 2", n)
	}
	if err := r.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	tasks := r.fake.Tasks()
	if len(tasks) != 2 || len(tasks[0].Comments) != 1 || len(tasks[1].Comments) != 1 {
		t.Fatalf("tasks = %+v, want two, one comment each", tasks)
	}
	if !strings.Contains(r.logs.String(), "event e1: task 1 (created false, commented false") {
		t.Fatalf("the resend was not answered as a repeat: %q", r.logs.String())
	}
}

func TestTheLimiterSpacesRequests(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := deliver.NewLimiter(60, 2, func() time.Time { return now }) // one a minute, burst 2
	var waits []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { waits = append(waits, d); now = now.Add(d); return nil }
	for range 4 {
		if err := l.Wait(context.Background(), sleep); err != nil {
			t.Fatal(err)
		}
	}
	if len(waits) != 2 || waits[0] != time.Minute || waits[1] != time.Minute {
		t.Fatalf("waits = %v, want two of 1m after a burst of 2", waits)
	}
}

func TestDrainWaitsOnTheLimiterBeforeEachEvent(t *testing.T) {
	r := newRig(t, t.TempDir())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := now
	r.d.Limiter = deliver.NewLimiter(6, 1, func() time.Time { return now }) // one per 10 min
	r.d.Sleep = func(_ context.Context, d time.Duration) error {
		r.sleeps = append(r.sleeps, d)
		now = now.Add(d)
		return nil
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		r.add(t, id, newTask("alert:"+id, id))
	}
	if err := r.d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.sleeps) < 2 || r.sleeps[0] != 10*time.Minute || now.Sub(start) < 20*time.Minute || r.fake.RequestCount() != 3 {
		t.Fatalf("waits = %v, requests %d", r.sleeps, r.fake.RequestCount())
	}
}
