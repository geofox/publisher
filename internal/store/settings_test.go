package store

import (
	"context"
	"errors"
	"testing"
)

func TestAccountSetsOneDefaultUniqueNamesConflicts(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	name, accts, yes := "Work", []string{"acc_a", "acc_b"}, true
	a, err := s.CreateAccountSet(ctx, DefaultOwner, AccountSetChange{Name: &name, Accounts: &accts, IsDefault: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccountSet(ctx, DefaultOwner, AccountSetChange{Name: &name, Accounts: &accts}); !errors.Is(err, ErrAccountSetName) {
		t.Fatalf("duplicate name: %v", err)
	}
	name2 := "Home"
	b, err := s.CreateAccountSet(ctx, DefaultOwner, AccountSetChange{Name: &name2, Accounts: &accts, IsDefault: &yes})
	if err != nil {
		t.Fatal(err)
	}
	all, err := s.AccountSets(ctx, DefaultOwner)
	if err != nil || len(all) != 2 {
		t.Fatalf("%v %v", all, err)
	}
	for _, x := range all {
		if x.IsDefault != (x.ID == b.ID) {
			t.Fatalf("default moved wrong: %+v", all)
		}
	}
	// A stale update loses; a fresh one wins.
	one := []string{"acc_a"}
	if _, err := s.UpdateAccountSet(ctx, DefaultOwner, a.ID, AccountSetChange{Accounts: &one, IfUpdatedAt: a.UpdatedAt.Add(-1)}); !errors.Is(err, ErrAccountSetConflict) {
		t.Fatalf("stale: %v", err)
	}
	got, err := s.UpdateAccountSet(ctx, DefaultOwner, a.ID, AccountSetChange{Accounts: &one, IfUpdatedAt: a.UpdatedAt})
	if err != nil || len(got.Accounts) != 1 || !got.UpdatedAt.After(a.UpdatedAt) {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.DeleteAccountSet(ctx, DefaultOwner, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccountSet(ctx, DefaultOwner, a.ID); !errors.Is(err, ErrAccountSetNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestApplySettingsRefusesUnsealedOrForeignSecrets(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	err := s.ApplySettings(ctx, SettingsChange{Owner: DefaultOwner, PutSecrets: []SealedSecret{
		{Owner: "other", Scope: SecretScopeSetting, Ref: "k", Name: "value", Sealed: "v1.x.y", KeyID: "x"}}})
	if err == nil {
		t.Fatal("foreign owner accepted")
	}
	if err := s.ApplySettings(ctx, SettingsChange{Owner: DefaultOwner, Put: map[string]string{"a.b": "{"}}); err == nil {
		t.Fatal("bad JSON accepted")
	}
	if err := s.ApplySettings(ctx, SettingsChange{Owner: DefaultOwner, Put: map[string]string{"a.b": "true"}, By: "session"}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.Settings(ctx, DefaultOwner)
	if err != nil || len(rows) != 1 || rows[0].ValueJSON != "true" || rows[0].UpdatedBy != "session" {
		t.Fatalf("%+v %v", rows, err)
	}
}

func TestAccountDefaultsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := Account{ID: "acc_d", OwnerID: DefaultOwner, Type: "mastodon", Label: "M", Source: SourceEnv, EnvKey: EnvKeyDefault, Enabled: true}
	if err := s.InsertAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.Account(ctx, DefaultOwner, "acc_d")
	if err != nil || got.Defaults != "{}" {
		t.Fatalf("%q %v", got.Defaults, err)
	}
	d := `{"visibility":"unlisted"}`
	up, err := s.UpdateAccountMeta(ctx, DefaultOwner, "acc_d", AccountMeta{Defaults: &d})
	if err != nil || up.Defaults != d {
		t.Fatalf("%+v %v", up, err)
	}
	got, _ = s.Account(ctx, DefaultOwner, "acc_d")
	if got.Defaults != d {
		t.Fatalf("stored %q", got.Defaults)
	}
}
