// Package outbox is the plugin's durable record of upstream events: every
// event is written here (and fsynced) BEFORE the plugin answers its upstream
// or moves its cursor, and is delivered to muster from here, in order. It is
// what makes "nothing is filed twice, nothing is lost" hold across restarts:
//
//   - Add refuses an event id it already holds, pending or delivered, so a
//     repeat of an upstream event (a re-polled issue, a repeated Alertmanager
//     notification) never becomes a second request to muster.
//   - An event added but not yet marked Done is delivered again after a
//     restart. muster's own idempotency (one open task per key, a comment
//     added once) makes that resend harmless if the first one did land.
//
// The file is append-only JSON lines (STATE_DIR/outbox.jsonl). On Open it is
// rewritten keeping every pending event and the ids of events delivered within
// the retention window, so it does not grow without bound. An event id older
// than the window is forgotten: choose a window longer than any upstream would
// repeat an event (the default is 30 days).
package outbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/atomicfile"
	"github.com/ZacxDev/muster-github-issues/internal/muster"
)

// FileName is the outbox file in the state directory.
const FileName = "outbox.jsonl"

// DefaultRetention is how long a delivered event's id is remembered.
const DefaultRetention = 30 * 24 * time.Hour

// MaxIDBytes bounds an event id.
const MaxIDBytes = 400

// Item is one pending event.
type Item struct {
	ID    string      `json:"id"`
	Task  muster.Task `json:"task"`
	Added time.Time   `json:"added"`
}

type record struct {
	Op   string       `json:"op"` // "add" or "done"
	ID   string       `json:"id"`
	Task *muster.Task `json:"task,omitempty"`
	At   time.Time    `json:"at"`
}

// Outbox is safe for concurrent use.
type Outbox struct {
	mu      sync.Mutex
	dir     string
	f       *os.File
	pending []Item               // in the order added
	done    map[string]time.Time // delivered id → when
	wake    chan struct{}
	now     func() time.Time
}

// Open reads (or creates) dir/outbox.jsonl, compacting it.
func Open(dir string, retention time.Duration, now func() time.Time) (*Outbox, error) {
	if now == nil {
		now = time.Now
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, FileName)
	o := &Outbox{dir: dir, done: map[string]time.Time{}, wake: make(chan struct{}, 1), now: now}
	raw, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := o.replay(p, raw); err != nil {
		return nil, err
	}
	// Compact: the pending events, and the delivered ids still in the window.
	cut := now().Add(-retention)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for id, at := range o.done {
		if at.Before(cut) {
			delete(o.done, id)
			continue
		}
		_ = enc.Encode(record{Op: "done", ID: id, At: at})
	}
	for _, it := range o.pending {
		t := it.Task
		_ = enc.Encode(record{Op: "add", ID: it.ID, Task: &t, At: it.Added})
	}
	if err := atomicfile.Write(dir, FileName, buf.Bytes()); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	o.f = f
	return o, nil
}

func (o *Outbox) replay(p string, raw []byte) error {
	lines := bytes.Split(raw, []byte("\n"))
	for i, l := range lines {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		var r record
		err := json.Unmarshal(l, &r)
		if err == nil && r.ID != "" && (r.Op == "done" || (r.Op == "add" && r.Task != nil)) {
			o.apply(r)
			continue
		}
		// The last line with no newline after it is a write a crash cut
		// short: its Add never returned, so the event was never acknowledged
		// upstream and dropping it loses nothing. Any other unreadable line
		// is damage: stop.
		if i == len(lines)-1 {
			return nil
		}
		return fmt.Errorf("outbox %s line %d is unreadable; fix or move it before starting", p, i+1)
	}
	return nil
}

func (o *Outbox) apply(r record) {
	switch r.Op {
	case "add":
		if _, ok := o.done[r.ID]; ok || o.indexPending(r.ID) >= 0 {
			return
		}
		o.pending = append(o.pending, Item{ID: r.ID, Task: *r.Task, Added: r.At})
	case "done":
		if i := o.indexPending(r.ID); i >= 0 {
			o.pending = append(o.pending[:i], o.pending[i+1:]...)
		}
		o.done[r.ID] = r.At
	}
}

func (o *Outbox) indexPending(id string) int {
	for i, it := range o.pending {
		if it.ID == id {
			return i
		}
	}
	return -1
}

func (o *Outbox) write(r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := o.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return o.f.Sync()
}

// Add records an event durably. It answers false, writing nothing, for an id
// it already holds (pending or delivered): that is the dedupe.
func (o *Outbox) Add(id string, t muster.Task) (bool, error) {
	if id == "" || len(id) > MaxIDBytes {
		return false, fmt.Errorf("outbox: event id must be 1-%d bytes", MaxIDBytes)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, done := o.done[id]; done || o.indexPending(id) >= 0 {
		return false, nil
	}
	r := record{Op: "add", ID: id, Task: &t, At: o.now().UTC()}
	if err := o.write(r); err != nil {
		return false, fmt.Errorf("outbox: %w", err)
	}
	o.apply(r)
	select {
	case o.wake <- struct{}{}:
	default:
	}
	return true, nil
}

// Done marks a pending event delivered, durably.
func (o *Outbox) Done(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.indexPending(id) < 0 {
		return nil
	}
	r := record{Op: "done", ID: id, At: o.now().UTC()}
	if err := o.write(r); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	o.apply(r)
	return nil
}

// Pending is a copy of the pending events, oldest first.
func (o *Outbox) Pending() []Item {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Item(nil), o.pending...)
}

// Len is the number of pending events.
func (o *Outbox) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}

// Wake is signalled (without blocking) whenever an event is added.
func (o *Outbox) Wake() <-chan struct{} { return o.wake }

// Close closes the file.
func (o *Outbox) Close() error { return o.f.Close() }
