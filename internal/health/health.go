// Package health runs readiness checks for GET /readyz.
//
// Local checks (database, disk, work directories) decide whether this
// instance can accept and persist work; a failing critical check makes the
// instance unavailable (HTTP 503). External checks (upstream reachability)
// run only on request, are never critical, and are cached so that probes
// can't turn into traffic at other people's servers.
package health

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"
)

// Overall statuses.
const (
	StatusOK          = "ok"          // every check passed
	StatusDegraded    = "degraded"    // a non-critical check failed; still serving
	StatusUnavailable = "unavailable" // a critical check failed; not ready
)

// Per-check statuses.
const (
	CheckOK   = "ok"
	CheckFail = "fail"
)

// DefaultTimeout bounds a check that sets no Timeout.
const DefaultTimeout = 2 * time.Second

// DefaultExternalTTL is how long external results are reused.
const DefaultExternalTTL = 30 * time.Second

// Check is one named readiness probe. Run returns nil when healthy. Wrap a
// failure with Fail to choose the short, public error class shown in the
// report; anything else is reported as "error" (or "timeout"). The raw error
// is only logged, never returned to the caller of /readyz.
type Check struct {
	Name     string
	Critical bool
	Timeout  time.Duration
	Run      func(ctx context.Context) error
}

// Problem is a check failure with a public class ("timeout", "unreachable",
// "http 503", "schema mismatch", …) and the private underlying error.
type Problem struct {
	Class string
	Err   error
}

func (p *Problem) Error() string {
	if p.Err == nil {
		return p.Class
	}
	return p.Class + ": " + p.Err.Error()
}

func (p *Problem) Unwrap() error { return p.Err }

// Fail wraps err with a public error class.
func Fail(class string, err error) error { return &Problem{Class: class, Err: err} }

// Result is one check's outcome as reported by /readyz.
type Result struct {
	Status     string `json:"status"`
	Critical   bool   `json:"critical"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Report is the /readyz response body.
type Report struct {
	Status string            `json:"status"`
	Checks map[string]Result `json:"checks"`
}

// Checker runs local checks on every call and external checks on request.
type Checker struct {
	Local       []Check
	External    []Check
	ExternalTTL time.Duration // 0 → DefaultExternalTTL

	now func() time.Time

	extMu    sync.Mutex // serializes external runs: one in-flight probe set
	cacheMu  sync.Mutex
	extCache map[string]Result
	extAt    time.Time

	logMu sync.Mutex
	last  map[string]string // check name → last status, for change logging
}

// Run executes the checks and builds the report.
func (c *Checker) Run(ctx context.Context, external bool) Report {
	results := c.runAll(ctx, c.Local)
	if external && len(c.External) > 0 {
		for name, r := range c.external() {
			results[name] = r
		}
	}
	rep := Report{Status: StatusOK, Checks: results}
	for _, r := range results {
		if r.Status == CheckOK {
			continue
		}
		if r.Critical {
			rep.Status = StatusUnavailable
		} else if rep.Status == StatusOK {
			rep.Status = StatusDegraded
		}
	}
	return rep
}

func (c *Checker) clock() func() time.Time {
	if c.now != nil {
		return c.now
	}
	return time.Now
}

// external returns cached external results, refreshing them when stale. Only
// one refresh runs at a time; callers that arrive meanwhile wait for it and
// share its results.
func (c *Checker) external() map[string]Result {
	ttl := c.ExternalTTL
	if ttl <= 0 {
		ttl = DefaultExternalTTL
	}
	if r, ok := c.cached(ttl); ok {
		return r
	}
	c.extMu.Lock()
	defer c.extMu.Unlock()
	if r, ok := c.cached(ttl); ok {
		return r
	}
	// Detached from the request: a caller hanging up must not poison the
	// shared cache with "timeout" results. Each check still has its timeout.
	r := c.runAll(context.Background(), c.External)
	c.cacheMu.Lock()
	c.extCache, c.extAt = r, c.clock()()
	c.cacheMu.Unlock()
	return r
}

func (c *Checker) cached(ttl time.Duration) (map[string]Result, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.extCache == nil || c.clock()().Sub(c.extAt) >= ttl {
		return nil, false
	}
	out := make(map[string]Result, len(c.extCache))
	for k, v := range c.extCache {
		out[k] = v
	}
	return out, true
}

type outcome struct {
	name   string
	result Result
	err    error
}

func (c *Checker) runAll(ctx context.Context, checks []Check) map[string]Result {
	now := c.clock()
	ch := make(chan outcome, len(checks))
	for _, chk := range checks {
		go func(chk Check) {
			timeout := chk.Timeout
			if timeout <= 0 {
				timeout = DefaultTimeout
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := now()
			err := runSafely(cctx, chk)
			r := Result{Status: CheckOK, Critical: chk.Critical, DurationMS: now().Sub(start).Milliseconds()}
			if err != nil {
				r.Status = CheckFail
				r.Error = classify(cctx, err)
			}
			ch <- outcome{name: chk.Name, result: r, err: err}
		}(chk)
	}
	out := make(map[string]Result, len(checks))
	for range checks {
		o := <-ch
		out[o.name] = o.result
		c.logChange(o)
	}
	return out
}

// runSafely runs the check, converting a panic into a failure, and returns as
// soon as the context ends even if the check ignores it.
func runSafely(ctx context.Context, chk Check) error {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- Fail("error", errors.New("check panicked"))
			}
		}()
		done <- chk.Run(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func classify(ctx context.Context, err error) string {
	var p *Problem
	if errors.As(err, &p) && p.Class != "" {
		return p.Class
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded ||
		(errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	return "error"
}

// logChange logs a check's transitions (and its first failure) once, so a
// probe every few seconds doesn't flood the log.
func (c *Checker) logChange(o outcome) {
	c.logMu.Lock()
	if c.last == nil {
		c.last = map[string]string{}
	}
	prev, seen := c.last[o.name]
	c.last[o.name] = o.result.Status
	c.logMu.Unlock()
	switch {
	case o.result.Status == CheckFail && prev != CheckFail:
		slog.Warn("readiness check failing", "check", o.name, "critical", o.result.Critical, "err", o.err)
	case o.result.Status == CheckOK && seen && prev == CheckFail:
		slog.Info("readiness check recovered", "check", o.name)
	}
}

// Names returns the configured check names, sorted (for logs and docs).
func (c *Checker) Names() (local, external []string) {
	for _, k := range c.Local {
		local = append(local, k.Name)
	}
	for _, k := range c.External {
		external = append(external, k.Name)
	}
	sort.Strings(local)
	sort.Strings(external)
	return local, external
}
