package settings

import (
	"context"
	"encoding/json"
	"maps"
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

func keys(items []Item) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Key)
	}
	return out
}

func whys(items []Item) map[string]string {
	out := map[string]string{}
	for _, it := range items {
		out[it.Key] = it.Why
	}
	return out
}

// Importing leaves what the environment sets here and what isn't a
// setting here; a value refused here is left out with what is sent to
// it, and the rest is saved. The plan says the same, before.
func TestImport(t *testing.T) {
	ctx := context.Background()
	s := newService(t, openStore(t), map[string]string{"SCHEDULE_GRACE": "3h"})
	vals := map[string]any{}
	// As a file holds them: JSON's numbers and lists.
	if err := json.Unmarshal([]byte(`{"publishing.max_attempts":4,"publishing.schedule_grace":"1h","composing.languages":["en","fr"],
		"alerts.webhook_url":"http://10.0.0.1/hook","alerts.webhook_pass":"pw","feed.token":"tok-1","media.public":true,"no.such":1}`), &vals); err != nil {
		t.Fatal(err)
	}
	want := []string{ComposingLanguages, FeedToken, PublishingMaxAttempts}
	set, skipped := s.PlanImport(ctx, Owner, vals)
	why := whys(skipped)
	if !slices.Equal(keys(set), want) || len(why) != 5 || why[PublishingGrace] != "set by SCHEDULE_GRACE in this server's environment" ||
		why["no.such"] != whyNotHere || why[MediaPublic] != whyNotHere || why[AlertsWebhookURL] == "" ||
		why[AlertsWebhookPass] != "goes with alerts.webhook_url, which wasn't saved" {
		t.Fatalf("plan %v, skipped %v", set, why)
	}
	if cur := s.Current(); cur.Int(PublishingMaxAttempts) == 4 {
		t.Fatal("the plan saved")
	}

	saved, skipped, err := s.Import(ctx, Owner, vals, "session")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys(saved), want) || !maps.Equal(whys(skipped), why) {
		t.Fatalf("saved %v, skipped %v", saved, whys(skipped))
	}
	cur := s.Current()
	if cur.Int(PublishingMaxAttempts) != 4 || cur.Secret(FeedToken) != "tok-1" || cur.Secret(AlertsWebhookPass) != "" ||
		!slices.Equal(cur.Strings(ComposingLanguages), []string{"en", "fr"}) {
		t.Fatal("not saved as planned")
	}

	// A value refused here is refused in the plan too.
	if set, skipped := s.PlanImport(ctx, Owner, map[string]any{PublishingMaxAttempts: 99}); len(set) != 0 || len(skipped) != 1 {
		t.Fatalf("plan of a refused value: %v %v", set, skipped)
	}

	// Blossom chosen without its address: the choice is left as it is.
	// Serving media publicly waits for its test, here.
	saved, skipped, err = s.Import(ctx, Instance, map[string]any{MediaStore: StoreBlossom, MediaPublic: true, MediaLocalQuota: 512}, "session")
	if err != nil || !slices.Equal(keys(saved), []string{MediaLocalQuota}) || whys(skipped)[MediaPublic] != whyMediaPublic ||
		whys(skipped)[MediaStore] == "" {
		t.Fatalf("instance: %v %v %v", saved, skipped, err)
	}
	if cur := s.Current(); cur.String(MediaStore) != StoreNone || cur.Bool(MediaPublic) {
		t.Fatal("instance not saved as planned")
	}
}

// A webhook's secret comes only with its address from the file: never
// to the address this server has.
func TestImportPairedSecrets(t *testing.T) {
	ctx := context.Background()
	// The environment sets the alerts' address here.
	s := newService(t, openStore(t), map[string]string{"ALERT_WEBHOOK_URL": "https://ops.example/hook"})
	for name, vals := range map[string]map[string]any{
		"the secret alone":            {AlertsWebhookPass: "file-pw"},
		"its address locked here too": {AlertsWebhookURL: "https://alerts.example/hook", AlertsWebhookPass: "file-pw"},
	} {
		set, skipped := s.PlanImport(ctx, Owner, vals)
		saved, skipped2, err := s.Import(ctx, Owner, vals, "session")
		if err != nil || len(set) != 0 || len(saved) != 0 || whys(skipped)[AlertsWebhookPass] == "" || !maps.Equal(whys(skipped), whys(skipped2)) {
			t.Fatalf("%s: plan %v %v, import %v %v %v", name, set, skipped, saved, skipped2, err)
		}
		if got := s.Current().Secret(AlertsWebhookPass); got != "" {
			t.Fatalf("%s: the password was saved for the environment's address", name)
		}
	}

	// Saved here for one address; the file has another, without a
	// secret: the address is taken, and the old secret goes with it.
	s = newService(t, openStore(t), nil)
	if _, err := s.Apply(ctx, Change{Scope: Owner, Values: map[string]any{FeedWebhookURL: "https://old.example/hook", FeedWebhookToken: "old-tok"}}); err != nil {
		t.Fatal(err)
	}
	vals := map[string]any{FeedWebhookURL: "https://new.example/hook", PublishingMaxAttempts: 4, PublishingGrace: "2h"}
	set, _ := s.PlanImport(ctx, Owner, vals)
	saved, skipped, err := s.Import(ctx, Owner, vals, "session")
	want := []string{FeedWebhookToken, FeedWebhookURL, PublishingMaxAttempts, PublishingGrace}
	if err != nil || len(skipped) != 0 || !slices.Equal(keys(set), want) || !slices.Equal(keys(saved), want) ||
		whys(saved)[FeedWebhookToken] == "" {
		t.Fatalf("plan %v, saved %v, skipped %v, %v", set, saved, skipped, err)
	}
	if cur := s.Current(); cur.Secret(FeedWebhookURL) != "https://new.example/hook" || cur.Secret(FeedWebhookToken) != "" || cur.Int(PublishingMaxAttempts) != 4 {
		t.Fatal("not saved as planned")
	}
}
