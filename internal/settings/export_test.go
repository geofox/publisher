package settings

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// Saved is what the app saved, secrets opened; not the environment's
// values or the defaults.
func TestSaved(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	s := newService(t, st, map[string]string{"SCHEDULE_GRACE": "3h"})
	if _, err := s.Apply(ctx, Change{Scope: Owner, Values: map[string]any{PublishingMaxAttempts: 3,
		AlertsWebhookURL: "https://alerts.example/hook?k=s3cret", AlertsWebhookPass: "pw-1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, Change{Scope: Instance, Values: map[string]any{MediaPublic: true}}); err != nil {
		t.Fatal(err)
	}
	cur := s.Current()
	owner := cur.Saved(Owner)
	if len(owner) != 3 || owner[PublishingMaxAttempts] != 3 || owner[AlertsWebhookURL] != "https://alerts.example/hook?k=s3cret" ||
		owner[AlertsWebhookPass] != "pw-1" {
		t.Fatalf("owner: %v", owner)
	}
	if inst := cur.Saved(Instance); len(inst) != 1 || inst[MediaPublic] != true {
		t.Fatalf("instance: %v", inst)
	}
	// A secret that can't be opened (another key) isn't exported.
	other := newServiceKeys(t, st, nil, "settings-test-other-key-32-bytes")
	if got := other.Current().Saved(Owner); got[AlertsWebhookPass] != nil || got[AlertsWebhookURL] != nil || got[PublishingMaxAttempts] != 3 {
		t.Fatalf("with another key: %v", got)
	}
}

// Importing leaves what the environment sets here and what isn't a
// setting here; a value refused here is left out with what is sent to
// it, and the rest is saved.
func TestImport(t *testing.T) {
	ctx := context.Background()
	s := newService(t, openStore(t), map[string]string{"SCHEDULE_GRACE": "3h"})
	vals := map[string]any{}
	// As a file holds them: JSON's numbers and lists.
	if err := json.Unmarshal([]byte(`{"publishing.max_attempts":4,"publishing.schedule_grace":"1h","composing.languages":["en","fr"],
		"alerts.webhook_url":"http://10.0.0.1/hook","alerts.webhook_pass":"pw","feed.token":"tok-1","media.public":true,"no.such":1}`), &vals); err != nil {
		t.Fatal(err)
	}
	set, skipped := s.PlanImport(Owner, vals)
	if !slices.Equal(set, []string{AlertsWebhookPass, AlertsWebhookURL, ComposingLanguages, FeedToken, PublishingMaxAttempts}) {
		t.Fatalf("plan set %v", set)
	}
	why := map[string]string{}
	for _, k := range skipped {
		why[k.Key] = k.Why
	}
	if len(why) != 3 || why[PublishingGrace] != "set by SCHEDULE_GRACE in this server's environment" ||
		why["no.such"] != "not a setting on this server" || why[MediaPublic] != "not a setting on this server" {
		t.Fatalf("plan skipped %v", why)
	}

	saved, skipped, err := s.Import(ctx, Owner, vals, "session")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved, []string{ComposingLanguages, FeedToken, PublishingMaxAttempts}) {
		t.Fatalf("saved %v", saved)
	}
	why = map[string]string{}
	for _, k := range skipped {
		why[k.Key] = k.Why
	}
	if why[AlertsWebhookURL] == "" || why[AlertsWebhookPass] != "goes with alerts.webhook_url, which wasn't saved" {
		t.Fatalf("skipped %v", why)
	}
	cur := s.Current()
	if cur.Int(PublishingMaxAttempts) != 4 || cur.Secret(FeedToken) != "tok-1" || cur.Secret(AlertsWebhookPass) != "" ||
		!slices.Equal(cur.Strings(ComposingLanguages), []string{"en", "fr"}) {
		t.Fatal("not saved as planned")
	}

	// Blossom chosen without its address: the choice is left as it is.
	saved, skipped, err = s.Import(ctx, Instance, map[string]any{MediaStore: StoreBlossom, MediaPublic: true}, "session")
	if err != nil || !slices.Equal(saved, []string{MediaPublic}) || len(skipped) != 1 || skipped[0].Key != MediaStore {
		t.Fatalf("instance: %v %v %v", saved, skipped, err)
	}
	if cur := s.Current(); cur.String(MediaStore) != StoreNone || !cur.Bool(MediaPublic) {
		t.Fatal("instance not saved as planned")
	}
}
