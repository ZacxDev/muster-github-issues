package poller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/atomicfile"
)

// StateFile holds the per-repository cursors and what the plugin knows of
// each issue it filed.
const StateFile = "github-state.json"

// Known is what the plugin last recorded about one issue.
type Known struct {
	State     string    `json:"state"` // "open" or "closed"
	UpdatedAt time.Time `json:"updated_at"`
	// Filed: a task was filed for it. An issue seen only closed, before any
	// task, is remembered (so a reopen is filed) but is not filed.
	Filed bool `json:"filed,omitempty"`
}

// ForgetClosedAfter is how long the plugin remembers an issue that is closed:
// past it (counted from the issue's last update) the issue is dropped from the
// state, which otherwise grows by one entry per issue forever. A dropped issue
// that is reopened later is filed as a new issue.
const ForgetClosedAfter = 90 * 24 * time.Hour

// State is the poller's durable state. Not safe for concurrent use: the
// poller is one loop.
type State struct {
	dir string
	// Cursors is, per "owner/name", the latest updated_at already recorded
	// in the outbox: the next poll asks for issues updated at or after it.
	Cursors map[string]time.Time `json:"cursors"`
	// Issues is, per task key, the issue as last recorded.
	Issues map[string]Known `json:"issues"`
}

// OpenState reads dir/github-state.json (none yet: empty).
func OpenState(dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &State{dir: dir, Cursors: map[string]time.Time{}, Issues: map[string]Known{}}
	b, err := os.ReadFile(filepath.Join(dir, StateFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s is unreadable; fix or move it before starting", filepath.Join(dir, StateFile))
	}
	if s.Cursors == nil {
		s.Cursors = map[string]time.Time{}
	}
	if s.Issues == nil {
		s.Issues = map[string]Known{}
	}
	return s, nil
}

// Save writes the state durably.
func (s *State) Save() error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.dir, StateFile, b)
}

// ForgetClosed drops every closed issue last updated more than age before
// now, and says how many it dropped. It does not save.
func (s *State) ForgetClosed(now time.Time, age time.Duration) int {
	n := 0
	for k, i := range s.Issues {
		if i.State != "open" && now.Sub(i.UpdatedAt) > age {
			delete(s.Issues, k)
			n++
		}
	}
	return n
}
