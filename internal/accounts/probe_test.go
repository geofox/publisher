package accounts

import (
	"context"
	"reflect"
	"testing"

	"github.com/geofox/publisher/internal/health"
	"github.com/geofox/publisher/internal/store"
)

// The readiness probe checks every open account that is turned on, sends
// no credential, and never changes an account's status: a refused
// account stays refused until an authenticated check says otherwise.
func TestProbe(t *testing.T) {
	h := newHarness(t, []store.Account{
		row("acc_b", "bluesky", "env", "default", 0, true),
		row("acc_m", "mastodon", "env", "default", 0, false),
		row("acc_n", "nostr", "env", "default", 0, true),
		row("acc_t", "threads", "db", "", 0, true),
	}, []EnvAccount{env("bluesky", "default"), env("mastodon", "default"), env("nostr", "default")})
	ctx := context.Background()

	no := false
	h.live("acc_n").authOK = &no
	h.CheckNow(ctx, "acc_n")
	if e, _ := h.Get("acc_n"); e.Status != NeedsReconnect {
		t.Fatalf("acc_n = %s, want needs_reconnect", e.Status)
	}

	if got, want := h.Probeable(store.DefaultOwner), []string{"acc_b", "acc_n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Probeable = %v, want %v (turned off and locked accounts left out)", got, want)
	}
	if got := h.Probeable("someone-else"); len(got) != 0 {
		t.Errorf("another owner's probes = %v", got)
	}

	hb, ok := h.Probe(ctx, "acc_b")
	if !ok || !hb.OK || h.live("acc_b").lastReq.Authenticated {
		t.Errorf("acc_b probe = %+v ok=%v, authenticated=%v", hb, ok, h.live("acc_b").lastReq.Authenticated)
	}

	yes := true
	h.live("acc_n").authOK = &yes
	if _, ok := h.Probe(ctx, "acc_n"); !ok {
		t.Error("an open refused account wasn't probed")
	}
	if e, _ := h.Get("acc_n"); e.Status != NeedsReconnect {
		t.Errorf("after a probe acc_n = %s; only an authenticated check may clear needs_reconnect", e.Status)
	}

	for _, id := range []string{"acc_m", "acc_t", "acc_missing"} {
		if _, ok := h.Probe(ctx, id); ok {
			t.Errorf("%s was probed", id)
		}
	}
	if n := h.live("acc_m").checks; n != 0 {
		t.Errorf("the turned-off account was checked %d times", n)
	}
}

// A turned-off account keeps its stored handle and isn't asked for its
// profile (its credentials aren't used while it is off).
func TestProfileOfTurnedOffAccount(t *testing.T) {
	r := row("acc_m", "mastodon", "env", "default", 0, false)
	r.Handle = "me@m.example"
	h := newHarness(t, []store.Account{r}, []EnvAccount{env("mastodon", "default")})
	h.RefreshProfiles()
	p, ok := h.Profile("acc_m")
	h.wait()
	a := h.live("acc_m")
	a.mu.Lock()
	n := a.idents
	a.mu.Unlock()
	if !ok || p.Handle != "me@m.example" || n != 0 {
		t.Errorf("profile = %+v ok=%v, network asked %d times", p, ok, n)
	}
}

// Readiness has one non-critical check per open account that is turned
// on; a failure shows its class only, never the network's message.
func TestReadiness(t *testing.T) {
	h := newHarness(t, []store.Account{
		row("acc_b", "bluesky", "env", "default", 0, true),
		row("acc_m", "mastodon", "env", "default", 0, false),
		row("acc_n", "nostr", "env", "default", 0, true),
	}, []EnvAccount{env("bluesky", "default"), env("mastodon", "default"), env("nostr", "default")})
	h.live("acc_n").setAuth(nil) // the fake's "did not answer" check
	c := &health.Checker{MoreExternal: func() []health.Check { return h.Readiness(store.DefaultOwner) }}
	rep := c.Run(context.Background(), true)
	if len(rep.Checks) != 2 || rep.Status != health.StatusDegraded {
		t.Fatalf("report = %+v", rep)
	}
	if b := rep.Checks["account:acc_b"]; b.Status != health.CheckOK || b.Critical {
		t.Errorf("acc_b = %+v", b)
	}
	if n := rep.Checks["account:acc_n"]; n.Status != health.CheckFail || n.Error != "unreachable" || n.Critical {
		t.Errorf("acc_n = %+v, want a non-critical failure classed unreachable", n)
	}
	if h.live("acc_b").lastReq.Authenticated || h.live("acc_n").lastReq.Authenticated {
		t.Error("a readiness probe sent credentials")
	}
}
