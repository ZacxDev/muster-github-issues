package main

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

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{"MUSTER_URL": "http://127.0.0.1:8105", "MUSTER_PLUGIN_TOKEN": "mpl_x",
		"GITHUB_TOKEN": "github_pat_x", "GITHUB_REPOS": "example-org/widgets, example-org/gadgets"}
}

func with(kv ...string) map[string]string {
	m := base()
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := LoadConfig(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.APIURL != "https://api.github.com" || len(c.Repos) != 2 || c.Repos[1].Name != "gadgets" || c.Interval != time.Minute ||
		c.Namespace != "github" || c.StateDir != "./state" || !c.Since.IsZero() || len(c.Labels) != 0 || c.PerHour != 60 || c.Burst != 10 {
		t.Fatalf("defaults = %+v", c)
	}
	c, err = LoadConfig(env(with("GITHUB_LABELS", "agent, bug", "GITHUB_POLL_INTERVAL", "30s", "GITHUB_SINCE", "2026-11-01T00:00:00Z",
		"GITHUB_TASK_NAMESPACE", "gh", "GITHUB_API_URL", "https://github.example.com/api/v3/", "STATE_DIR", "/tmp/s",
		"GITHUB_MAX_PER_HOUR", "7", "GITHUB_BURST", "3")))
	if err != nil || strings.Join(c.Labels, "|") != "agent|bug" || c.Interval != 30*time.Second || c.Namespace != "gh" ||
		c.APIURL != "https://github.example.com/api/v3" || c.Since.Day() != 1 || c.StateDir != "/tmp/s" || c.PerHour != 7 || c.Burst != 3 {
		t.Fatalf("set = %+v, %v", c, err)
	}
}

func TestLoadConfigRefuses(t *testing.T) {
	for _, bad := range []map[string]string{
		with("MUSTER_URL", ""),
		with("MUSTER_URL", "http://muster.example.com"),
		with("MUSTER_PLUGIN_TOKEN", ""),
		with("GITHUB_TOKEN", ""),
		with("GITHUB_REPOS", ""),
		with("GITHUB_REPOS", "widgets"),
		with("GITHUB_REPOS", "a/b/c"),
		with("GITHUB_POLL_INTERVAL", "1s"),
		with("GITHUB_SINCE", "yesterday"),
		with("GITHUB_TASK_NAMESPACE", "GitHub"),
		with("GITHUB_API_URL", "http://github.example.com"),
		with("GITHUB_MAX_PER_HOUR", "0"),
		with("GITHUB_BURST", "many"),
	} {
		if _, err := LoadConfig(env(bad)); err == nil {
			t.Errorf("LoadConfig(%v) accepted it", bad)
		}
	}
}

func TestTheStartupLineNamesNoSecret(t *testing.T) {
	c, err := LoadConfig(env(with("MUSTER_PLUGIN_TOKEN", "mpl_s3cretT0ken", "GITHUB_TOKEN", "github_pat_s3cret")))
	if err != nil {
		t.Fatal(err)
	}
	got := startupLine(c, 2)
	want := `muster-github-issues: talking to http://127.0.0.1:8105; polling example-org/widgets, example-org/gadgets every 1m0s (labels: any); namespace "github"; at most 60 tasks/hour (burst 10); 2 events pending`
	if got != want {
		t.Fatalf("startup line = %q\nwant %q", got, want)
	}
	if strings.Contains(got, "s3cret") {
		t.Fatalf("a token is in the startup line: %q", got)
	}
}

// The namespace check at startup, against the fake muster, which answers GET
// /plugin/v1/tasks as muster's handlePluginTaskList does: 400 for a namespace
// not in MUSTER_PLUGIN_TASK_NAMESPACES.
func TestPreflightRefusesANamespaceMusterDoesNotList(t *testing.T) {
	fake := mustertest.New()
	defer fake.Close()
	c, _ := muster.New(fake.URL, mustertest.Token)
	var logs bytes.Buffer
	cfg, err := LoadConfig(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if err := preflight(context.Background(), c, cfg, log.New(&logs, "", 0)); err != nil {
		t.Fatalf("preflight with a listed namespace = %v", err)
	}
	cfg.Namespace = "issues"
	err = preflight(context.Background(), c, cfg, log.New(&logs, "", 0))
	want := `muster does not accept the task namespace "issues" (set by GITHUB_TASK_NAMESPACE): add it to MUSTER_PLUGIN_TASK_NAMESPACES on the muster server`
	if err == nil || !errors.Is(err, deliver.ErrStop) || !strings.Contains(err.Error(), want) {
		t.Fatalf("preflight with an unlisted namespace = %v\nwant it to contain %q", err, want)
	}
	if n := fake.RequestCount(); n != 0 {
		t.Fatalf("preflight posted %d tasks", n)
	}
}

// The delivery limiter is wired in: a backfill reaches muster at
// GITHUB_MAX_PER_HOUR after the first GITHUB_BURST, not all at once.
func TestTheDelivererIsRateLimited(t *testing.T) {
	fake := mustertest.New()
	defer fake.Close()
	c, _ := muster.New(fake.URL, mustertest.Token)
	box, err := outbox.Open(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	for _, k := range []string{"a", "b", "c", "d"} {
		box.Add(k, muster.Task{Key: "github:x/y/" + k, Create: &muster.TaskCreate{Title: k, Body: k}, Comment: "[" + k + "] new"})
	}
	cfg, _ := LoadConfig(env(with("GITHUB_MAX_PER_HOUR", "60", "GITHUB_BURST", "2")))
	var logs bytes.Buffer
	d := newDeliverer(cfg, c, box, log.New(&logs, "", 0))
	var waited time.Duration
	d.Sleep = func(ctx context.Context, w time.Duration) error { waited = w; return context.Canceled }
	_ = d.Drain(context.Background())
	if n := fake.RequestCount(); n != 2 || waited < 55*time.Second || waited > time.Minute {
		t.Fatalf("delivered %d then waited %s; want the burst of 2, then about a minute", n, waited)
	}
}
