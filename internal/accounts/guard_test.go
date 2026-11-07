package accounts

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// listener counts the connections that reach it.
func listener(t *testing.T) (addr string, conns *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), conns
}

// An account added in the app whose server is this machine never reaches
// it, by address or by name, whatever the driver does with it (a check, a
// profile lookup); the same address set by the operator in the
// environment is reached as before. Threads always goes to graph.threads.net,
// so it has no address to point here.
func TestDBAccountsDialThroughTheGuard(t *testing.T) {
	drivers.Register()
	ctx := context.Background()
	const nsec = "nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5"
	cases := []struct {
		typ, method string
		config      func(host string) map[string]any
		secrets     map[string]string
	}{
		{"bluesky", "app_password", func(h string) map[string]any {
			return map[string]any{"pds_url": "https://" + h, "identifier": "ada.example"}
		}, map[string]string{"app_password": "abcd-efgh-ijkl-mnop"}},
		{"mastodon", "token", func(h string) map[string]any {
			return map[string]any{"instance_url": "https://" + h}
		}, map[string]string{"access_token": "tok"}},
		{"nostr", "secret_key", func(h string) map[string]any {
			return map[string]any{"bootstrap_relay": "wss://" + h, "fallback_relays": []string{"wss://" + h}}
		}, map[string]string{"secret_key": nsec}},
	}
	for _, c := range cases {
		addr, conns := listener(t)
		_, port, _ := net.SplitHostPort(addr)
		for _, host := range []string{addr, "localhost:" + port} {
			t.Run(c.typ+"/"+host, func(t *testing.T) {
				kr := secrets.Keyring{Current: testKey(t, 1)}
				st, _ := openStore(t)
				acct := addDB(t, st, kr, c.typ, c.config(host), c.secrets)
				ks, err := PrepareKeys(ctx, st, kr)
				if err != nil {
					t.Fatal(err)
				}
				r := New(Options{Store: st, Secrets: st, Keys: kr, KeyState: ks})
				if err := r.Load(ctx); err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				if e, _ := r.Get(acct.ID); e.Status == Locked || e.Status == NotConfigured {
					t.Fatalf("not opened: %s %q", e.Status, e.Reason)
				}
				h, _ := r.CheckNow(ctx, acct.ID)
				r.fetchProfile(ctx, acct.ID)
				r.bgWait.Wait()
				if n := conns.Load(); n != 0 {
					t.Errorf("%d connections reached this machine", n)
				}
				if h.OK {
					t.Errorf("check passed: %+v", h)
				}
				if strings.Contains(h.Message, "127.0.0.1") {
					t.Errorf("the check names the address: %q", h.Message)
				}
			})
		}
		// The control: the operator's account at the same address is
		// reached (so the listener counts what reaches it).
		t.Run(c.typ+"/env", func(t *testing.T) {
			st, _ := openStore(t)
			if _, err := st.SeedEnvAccounts(ctx, store.DefaultOwner, []store.EnvAccount{{Type: c.typ, Key: "default", Label: "Env"}}); err != nil {
				t.Fatal(err)
			}
			rows, _ := st.AllAccounts(ctx)
			r := New(Options{Store: st, Env: []EnvAccount{{Type: c.typ, EnvAccount: destination.EnvAccount{Key: "default",
				Config: c.config(addr), Secrets: c.secrets}}}})
			if err := r.Load(ctx); err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			before := conns.Load()
			r.CheckNow(ctx, rows[0].ID)
			if conns.Load() == before {
				t.Error("the operator's account didn't reach its address")
			}
		})
	}
}
