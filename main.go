// Command muster-github-issues turns GitHub issues into muster tasks. It polls
// the configured repositories with a read-only token and files one muster task
// per issue through POST /plugin/v1/tasks, routed to your chief; later changes
// to the issue become comments on that task.
//
// Configuration is read from the environment only (see .env.example and the
// README's table).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/deliver"
	"github.com/ZacxDev/muster-github-issues/internal/github"
	"github.com/ZacxDev/muster-github-issues/internal/muster"
	"github.com/ZacxDev/muster-github-issues/internal/outbox"
	"github.com/ZacxDev/muster-github-issues/internal/poller"
)

const name = "muster-github-issues"

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if err := run(logger); err != nil {
		logger.Printf("%s: %v", name, err)
		os.Exit(1)
	}
}

// Config is everything the plugin reads from its environment.
type Config struct {
	URL, Token  string
	GitHubToken string
	APIURL      string
	Repos       []github.Repo
	Labels      []string
	Interval    time.Duration
	Since       time.Time // zero: the first start's time
	Namespace   string
	StateDir    string
	PerHour     int // the delivery limiter: tasks/comments an hour
	Burst       int
}

func intEnv(getenv func(string) string, key string, def, lo, hi int) (int, error) {
	v := getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s %q must be a whole number from %d to %d", key, v, lo, hi)
	}
	return n, nil
}

var namespaceRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func list(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// LoadConfig reads and checks the environment through getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	base, err := muster.CheckBaseURL(getenv("MUSTER_URL"))
	if err != nil {
		return Config{}, err
	}
	c := Config{URL: base, Token: getenv("MUSTER_PLUGIN_TOKEN"), GitHubToken: getenv("GITHUB_TOKEN"),
		Interval: time.Minute, Namespace: "github", StateDir: "./state"}
	if c.Token == "" {
		return Config{}, errors.New("MUSTER_PLUGIN_TOKEN is not set (mint one on muster's /plugins page)")
	}
	if c.GitHubToken == "" {
		return Config{}, errors.New("GITHUB_TOKEN is not set (a fine-grained token with Issues: read on the repositories)")
	}
	api := github.DefaultAPI
	if v := getenv("GITHUB_API_URL"); v != "" {
		api = v
	}
	if c.APIURL, err = github.CheckAPIURL(api); err != nil {
		return Config{}, err
	}
	for _, s := range list(getenv("GITHUB_REPOS")) {
		r, err := github.ParseRepo(s)
		if err != nil {
			return Config{}, fmt.Errorf("GITHUB_REPOS: %w", err)
		}
		c.Repos = append(c.Repos, r)
	}
	if len(c.Repos) == 0 {
		return Config{}, errors.New("GITHUB_REPOS is not set (a comma-separated list of owner/name)")
	}
	c.Labels = list(getenv("GITHUB_LABELS"))
	if v := getenv("GITHUB_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Second {
			return Config{}, fmt.Errorf("GITHUB_POLL_INTERVAL %q is not a duration of at least 10s (e.g. 1m)", v)
		}
		c.Interval = d
	}
	if v := getenv("GITHUB_SINCE"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return Config{}, fmt.Errorf("GITHUB_SINCE %q is not an RFC 3339 time (e.g. 2026-11-01T00:00:00Z)", v)
		}
		c.Since = t
	}
	if v := getenv("GITHUB_TASK_NAMESPACE"); v != "" {
		c.Namespace = v
	}
	if !namespaceRE.MatchString(c.Namespace) {
		return Config{}, fmt.Errorf("GITHUB_TASK_NAMESPACE %q must be lower-case letters, digits, - or _ (it must also be in muster's MUSTER_PLUGIN_TASK_NAMESPACES)", c.Namespace)
	}
	if c.PerHour, err = intEnv(getenv, "GITHUB_MAX_PER_HOUR", 60, 1, 3600); err != nil {
		return Config{}, err
	}
	if c.Burst, err = intEnv(getenv, "GITHUB_BURST", 10, 1, 1000); err != nil {
		return Config{}, err
	}
	if v := getenv("STATE_DIR"); v != "" {
		c.StateDir = v
	}
	return c, nil
}

func run(logger *log.Logger) error {
	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	client, err := muster.New(cfg.URL, cfg.Token)
	if err != nil {
		return err
	}
	box, err := outbox.Open(cfg.StateDir, outbox.DefaultRetention, nil)
	if err != nil {
		return err
	}
	defer box.Close()
	state, err := poller.OpenState(cfg.StateDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := preflight(ctx, client, cfg, logger); err != nil {
		return err
	}
	gh := github.New(cfg.APIURL, cfg.GitHubToken)
	for _, r := range cfg.Repos {
		if err := gh.CheckRepo(ctx, r); err != nil {
			return fmt.Errorf("GITHUB_REPOS: %s: %v (does the token have Issues: read on it?)", r, err)
		}
	}
	since := cfg.Since
	if since.IsZero() {
		since = time.Now()
	}
	p := &poller.Poller{GH: gh, Repos: cfg.Repos, Labels: cfg.Labels, Namespace: cfg.Namespace, Since: since, Box: box, State: state, Log: logger}
	d := newDeliverer(cfg, client, box, logger)
	logger.Print(startupLine(cfg, box.Len()))

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan struct{}, 2)
	go func() { cancel(d.Run(ctx)); done <- struct{}{} }()
	go func() { cancel(p.Run(ctx, cfg.Interval, deliver.SleepCtx)); done <- struct{}{} }()
	<-done
	<-done
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// newDeliverer is the delivery loop under GITHUB_MAX_PER_HOUR / GITHUB_BURST:
// a backfill (GITHUB_SINCE far back) or a busy repository queues in the
// outbox and reaches the chief at that pace, instead of all at once.
func newDeliverer(cfg Config, client *muster.Client, box *outbox.Outbox, logger *log.Logger) *deliver.Deliverer {
	return &deliver.Deliverer{Client: client, Box: box, Log: logger, Limiter: deliver.NewLimiter(cfg.PerHour, cfg.Burst, nil)}
}

// preflight is what must hold before polling: the token works (GET
// /plugin/v1/self) and muster files tasks under the namespace.
func preflight(ctx context.Context, client *muster.Client, cfg Config, logger *log.Logger) error {
	if err := deliver.Hello(ctx, client, logger); err != nil {
		return err
	}
	return deliver.CheckNamespace(ctx, client, cfg.Namespace, "GITHUB_TASK_NAMESPACE")
}

// startupLine names muster by scheme and host only, and never a token.
func startupLine(cfg Config, pending int) string {
	repos := make([]string, len(cfg.Repos))
	for i, r := range cfg.Repos {
		repos[i] = r.String()
	}
	labels := "any"
	if len(cfg.Labels) > 0 {
		labels = "any of " + strings.Join(cfg.Labels, ", ")
	}
	return fmt.Sprintf("%s: talking to %s; polling %s every %s (labels: %s); namespace %q; at most %d tasks/hour (burst %d); %d events pending",
		name, muster.Redact(cfg.URL), strings.Join(repos, ", "), cfg.Interval, labels, cfg.Namespace, cfg.PerHour, cfg.Burst, pending)
}
