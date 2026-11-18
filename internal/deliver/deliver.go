// Package deliver drains the outbox into muster, one event at a time and in
// order, through POST /plugin/v1/tasks, optionally under a rate limit.
//
// What each of muster's answers does (docs/plugins.md, "POST /plugin/v1/tasks"
// and "Routing a task to the chief"):
//
//	201 / 200          delivered: marked done
//	400                the same request can never be accepted: logged, marked done
//	409                muster filed the task but will never route this event
//	                   (resending cannot help): logged, marked done
//	429                waits Retry-After, sends the same request again
//	503, 5xx, network  waits (Retry-After if given, else a doubling backoff), sends it again
//	401, 403, 404      stops the plugin: the token is revoked or lacks the
//	                   capability, or plugins are off on that server
//
// An event is marked done only after one of the answers above that ends it, so
// a crash or a stop mid-batch leaves it pending, and it is sent again on the
// next start. muster files one open task per key and adds a comment once, so
// a resend of a request that had in fact landed changes nothing.
package deliver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ZacxDev/muster-github-issues/internal/muster"
	"github.com/ZacxDev/muster-github-issues/internal/outbox"
)

// ErrStop wraps a refusal that no retry fixes.
var ErrStop = errors.New("stopping")

// Deliverer is the delivery loop. Client, Box and Log are required.
type Deliverer struct {
	Client  *muster.Client
	Box     *outbox.Outbox
	Log     *log.Logger
	Limiter *Limiter // nil: no rate limit
	// Sleep waits d or until ctx ends; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Backoff is the first wait after a network or server error (default
	// 5 s); it doubles up to MaxBackoff (default 5 min).
	Backoff, MaxBackoff time.Duration
	// Idle is how long Run waits for a new event before looking again
	// (default 5 s); an Add wakes it at once.
	Idle time.Duration
}

func (d *Deliverer) sleep(ctx context.Context, t time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, t)
	}
	return SleepCtx(ctx, t)
}

// SleepCtx waits d or until ctx ends.
func SleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// Fatal turns a refusal no retry fixes into ErrStop, or returns nil.
func Fatal(err error) error {
	e := muster.AsError(err)
	if e == nil {
		return nil
	}
	switch e.Kind {
	case muster.KindAuth:
		return fmt.Errorf("%w: muster refused the plugin token (revoked, unknown, or its owner is disabled): %v", ErrStop, e)
	case muster.KindCapability:
		return fmt.Errorf("%w: the plugin token lacks the messages capability: %v", ErrStop, e)
	case muster.KindOff:
		return fmt.Errorf("%w: muster answered 404: plugins are off on this server (set MUSTER_PLUGINS=1 there): %v", ErrStop, e)
	}
	return nil
}

// Hello is GET /plugin/v1/self, logged. It never logs the token.
func Hello(ctx context.Context, c *muster.Client, l *log.Logger) error {
	s, err := c.Self(ctx)
	if err != nil {
		if f := Fatal(err); f != nil {
			return f
		}
		return err
	}
	l.Printf("muster: plugin %q (id %d), owner %s, capabilities %v", s.PluginName, s.PluginID, s.OwnerLogin, s.Capabilities)
	return nil
}

// CheckNamespace asks muster whether ns is one of its plugin task namespaces
// (muster's GET /plugin/v1/tasks answers 400 when it is not), and refuses to
// start when it is not: every event would otherwise be refused, one by one,
// and dropped.
func CheckNamespace(ctx context.Context, c *muster.Client, ns, envVar string) error {
	err := c.ProbeNamespace(ctx, ns)
	if err == nil {
		return nil
	}
	if f := Fatal(err); f != nil {
		return f
	}
	if e := muster.AsError(err); e != nil && e.Kind == muster.KindBadRequest {
		return fmt.Errorf("%w: muster does not accept the task namespace %q (set by %s): add it to MUSTER_PLUGIN_TASK_NAMESPACES on the muster server and restart muster, or set %s to one it has (muster said: %s)",
			ErrStop, ns, envVar, envVar, e.Message)
	}
	return fmt.Errorf("checking the task namespace %q with muster: %w", ns, err)
}

// One delivers one event until muster gives an answer that ends it, then
// marks it done. It returns an error only to stop: ctx ended, or ErrStop.
func (d *Deliverer) One(ctx context.Context, it outbox.Item) error {
	wait := orDefault(d.Backoff, 5*time.Second)
	maxWait := orDefault(d.MaxBackoff, 5*time.Minute)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ack, err := d.Client.PostTask(ctx, it.Task)
		if err == nil {
			id := "none"
			if ack.ID != nil {
				id = fmt.Sprint(*ack.ID)
			}
			routed := ack.RoutedTo()
			if routed == "" {
				routed = "nowhere"
			}
			d.Log.Printf("event %s: task %s (created %t, commented %t, routed %s)", it.ID, id, ack.Created, ack.Commented, routed)
			return d.Box.Done(it.ID)
		}
		if f := Fatal(err); f != nil {
			return f
		}
		e := muster.AsError(err)
		switch {
		case e != nil && e.Kind == muster.KindRateLimited:
			d.Log.Printf("event %s: muster rate limited it; sending it again in %s", it.ID, e.RetryAfter)
			if err := d.sleep(ctx, e.RetryAfter); err != nil {
				return err
			}
		case e != nil && e.Retryable():
			t := max(wait, e.RetryAfter)
			d.Log.Printf("event %s: %v; sending it again in %s", it.ID, e, t)
			if err := d.sleep(ctx, t); err != nil {
				return err
			}
			wait = min(2*wait, maxWait)
		case e != nil && e.Kind == muster.KindConflict:
			d.Log.Printf("event %s: muster filed the task but will not route this event (%v); not sending it again", it.ID, e)
			return d.Box.Done(it.ID)
		default:
			d.Log.Printf("event %s: refused for good, dropped: %v", it.ID, err)
			return d.Box.Done(it.ID)
		}
	}
}

// Drain delivers every pending event, oldest first, waiting on the limiter
// before each. It returns when the outbox is empty, or with the error that
// stopped it.
func (d *Deliverer) Drain(ctx context.Context) error {
	for _, it := range d.Box.Pending() {
		if d.Limiter != nil {
			if err := d.Limiter.Wait(ctx, d.sleep); err != nil {
				return err
			}
		}
		if err := d.One(ctx, it); err != nil {
			return err
		}
	}
	return nil
}

// Run drains the outbox until ctx ends or a refusal stops it.
func (d *Deliverer) Run(ctx context.Context) error {
	idle := orDefault(d.Idle, 5*time.Second)
	for {
		if err := d.Drain(ctx); err != nil {
			return err
		}
		t := time.NewTimer(idle)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-d.Box.Wake():
		case <-t.C:
		}
		t.Stop()
	}
}
