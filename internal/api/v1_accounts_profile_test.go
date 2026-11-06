package api

import (
	"context"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/store"
)

// Every account shows its own profile, two of one network included; an
// avatar that isn't an absolute http(s) URL is left out, and an account
// whose network hasn't answered yet keeps its stored handle.
func TestV1AccountProfiles(t *testing.T) {
	e := newV1Env(t, true)
	if err := e.st.InsertAccount(context.Background(), store.Account{ID: "acc_work", Type: "mastodon", Label: "Work",
		Source: "env", EnvKey: "work", Handle: "stored@work.example", Enabled: true, CreatedAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	reg := e.withAccounts()
	home := e.acct("mastodon")
	e.api.Accounts = profiled{reg, map[string]accounts.Profile{
		home:       {Handle: "me@home.example", DisplayName: "Me at home", AvatarURL: "https://home.example/me.png"},
		"acc_work": {Handle: "stored@work.example", AvatarURL: "/avatars/original/missing.png"},
	}}
	r := e.do(req{method: "GET", path: "/v1/accounts"})
	wantStatus(t, r, 200)
	var got struct{ Data []v1AccountBody }
	r.json(&got)
	byID := map[string]v1AccountBody{}
	for _, a := range got.Data {
		byID[a.ID] = a
	}
	if a := byID[home]; a.Handle != "me@home.example" || a.DisplayName != "Me at home" || a.Avatar != "https://home.example/me.png" {
		t.Errorf("home account = %+v", a)
	}
	if a := byID["acc_work"]; a.Handle != "stored@work.example" || a.DisplayName != "" || a.Avatar != "" {
		t.Errorf("work account = %+v", a)
	}
	if a := byID[e.acct("bluesky")]; a.DisplayName != "" || a.Avatar != "" {
		t.Errorf("an account without a profile got one: %+v", a)
	}
	r = e.do(req{method: "GET", path: "/v1/accounts/" + home})
	wantStatus(t, r, 200)
	var one v1AccountBody
	r.json(&one)
	if one.DisplayName != "Me at home" {
		t.Errorf("single view = %+v", one)
	}
}
