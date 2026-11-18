package outbox_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/muster"
	"github.com/ZacxDev/muster-github-issues/internal/outbox"
)

func task(key, comment string) muster.Task { return muster.Task{Key: key, Comment: comment} }

func open(t *testing.T, dir string, now func() time.Time) *outbox.Outbox {
	t.Helper()
	o, err := outbox.Open(dir, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return o
}

// The dedupe: an event id the outbox holds, pending or delivered, is never
// added again, in this process or the next.
func TestADuplicateEventIsNeverAddedTwice(t *testing.T) {
	dir := t.TempDir()
	o := open(t, dir, nil)
	if ok, err := o.Add("ev-1", task("alert:a", "[ev-1] firing")); !ok || err != nil {
		t.Fatalf("first add = %t, %v", ok, err)
	}
	if ok, _ := o.Add("ev-1", task("alert:a", "[ev-1] firing, again")); ok {
		t.Fatal("a pending event id was added a second time")
	}
	if err := o.Done("ev-1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := o.Add("ev-1", task("alert:a", "[ev-1] firing")); ok {
		t.Fatal("a delivered event id was added a second time")
	}
	o.Close()
	o2 := open(t, dir, nil)
	if ok, _ := o2.Add("ev-1", task("alert:a", "[ev-1] firing")); ok {
		t.Fatal("after a restart, a delivered event id was added a second time")
	}
	if n := o2.Len(); n != 0 {
		t.Fatalf("pending after restart = %d, want 0", n)
	}
}

// Restart mid-batch: what was added and not marked done is pending again, in
// the order it was added, and what was done is not.
func TestPendingEventsSurviveARestartInOrder(t *testing.T) {
	dir := t.TempDir()
	o := open(t, dir, nil)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := o.Add(id, task("alert:"+id, "["+id+"]")); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Done("a"); err != nil {
		t.Fatal(err)
	}
	o.Close()
	o2 := open(t, dir, nil)
	var got []string
	for _, it := range o2.Pending() {
		got = append(got, it.ID+"="+it.Task.Comment)
	}
	if strings.Join(got, ",") != "b=[b],c=[c]" {
		t.Fatalf("pending after restart = %v, want b then c", got)
	}
}

// A last line a crash cut short is dropped (its Add never returned); a damaged
// line anywhere else stops the start.
func TestATornLastLineIsDroppedButDamageStops(t *testing.T) {
	dir := t.TempDir()
	o := open(t, dir, nil)
	if _, err := o.Add("a", task("alert:a", "[a]")); err != nil {
		t.Fatal(err)
	}
	o.Close()
	p := filepath.Join(dir, outbox.FileName)
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"op":"add","id":"b","ta`)
	f.Close()
	o2 := open(t, dir, nil)
	if n := o2.Len(); n != 1 {
		t.Fatalf("pending = %d, want 1 (the torn line dropped)", n)
	}
	o2.Close()

	os.WriteFile(p, []byte("not json\n"+`{"op":"done","id":"x","at":"2026-01-01T00:00:00Z"}`+"\n"), 0o600)
	if _, err := outbox.Open(dir, 0, nil); err == nil || !strings.Contains(err.Error(), "line 1 is unreadable") {
		t.Fatalf("damaged outbox opened: %v", err)
	}
}

// Compaction forgets a delivered id only after the retention window.
func TestCompactionKeepsDeliveredIDsForTheWindow(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	o, err := outbox.Open(dir, 48*time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	o.Add("old", task("alert:a", "[old]"))
	o.Done("old")
	o.Close()

	now = now.Add(47 * time.Hour)
	o, _ = outbox.Open(dir, 48*time.Hour, clock)
	if added, _ := o.Add("old", task("alert:a", "[old]")); added {
		t.Fatal("a delivered id was forgotten inside the window")
	}
	o.Close()

	now = now.Add(2 * time.Hour)
	o, _ = outbox.Open(dir, 48*time.Hour, clock)
	defer o.Close()
	if added, _ := o.Add("old", task("alert:a", "[old]")); !added {
		t.Fatal("a delivered id outlived the window")
	}
}
