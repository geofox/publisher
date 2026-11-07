package drivers

import (
	"context"
	"io"
	"maps"
	"regexp"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/destination/destinationtest"
	"github.com/geofox/publisher/internal/netguard"
	"github.com/geofox/publisher/internal/nostr"
)

// ownerURL is the host's check for addresses typed in the app.
func ownerURL(raw string, r destination.URLRule) (string, error) {
	return netguard.Policy{}.OwnerURL(raw, netguard.URLOptions{Secure: r.Secure, Plain: r.Plain, Path: r.Path})
}

var fieldTypes = map[destination.FieldType]bool{
	destination.FieldString: true, destination.FieldText: true, destination.FieldBool: true, destination.FieldInt: true,
	destination.FieldEnum: true, destination.FieldStringList: true, destination.FieldURL: true, destination.FieldURLList: true,
}

// Every type describes how to connect it, and its fields are sound: no
// secret in the config, audiences are addresses (one per type), keys
// don't collide, patterns compile, and every default validates.
func TestManifestFieldContract(t *testing.T) {
	Register()
	for _, typ := range destination.TypesInOrder() {
		d, _ := destination.Lookup(typ)
		m := d.Manifest()
		if len(m.Auth) == 0 {
			t.Errorf("%s: no auth method", typ)
		}
		audiences := 0
		check := func(where string, f destination.FieldSpec) {
			t.Helper()
			switch {
			case f.Key == "" || f.Label == "":
				t.Errorf("%s %s: field %+v has no key or label", typ, where, f)
			case !fieldTypes[f.Type]:
				t.Errorf("%s %s.%s: type %q", typ, where, f.Key, f.Type)
			case (f.Type == destination.FieldURL || f.Type == destination.FieldURLList) != (f.URL != nil):
				t.Errorf("%s %s.%s: a url field has a URL rule, and only one", typ, where, f.Key)
			case f.Audience && f.Type != destination.FieldURL:
				t.Errorf("%s %s.%s: an audience must be a url field", typ, where, f.Key)
			case f.Revealable && !f.Secret:
				t.Errorf("%s %s.%s: revealable but not secret", typ, where, f.Key)
			case f.Secret && f.Default != nil:
				t.Errorf("%s %s.%s: a secret with a default", typ, where, f.Key)
			case f.Type == destination.FieldEnum && len(f.Enum) == 0:
				t.Errorf("%s %s.%s: an enum without choices", typ, where, f.Key)
			}
			if f.Audience {
				audiences++
			}
			if f.Pattern != "" {
				if _, err := regexp.Compile(f.Pattern); err != nil {
					t.Errorf("%s %s.%s: pattern: %v", typ, where, f.Key, err)
				}
			}
			if f.Default != nil {
				if _, errs := destination.ValidateFields([]destination.FieldSpec{f}, map[string]any{f.Key: f.Default},
					destination.FieldOptions{URL: ownerURL}); len(errs) != 0 {
					t.Errorf("%s %s.%s: default doesn't validate: %+v", typ, where, f.Key, errs)
				}
			}
		}
		config := map[string]bool{}
		for _, f := range m.Config {
			if f.Secret {
				t.Errorf("%s: config field %s is secret", typ, f.Key)
			}
			if config[f.Key] {
				t.Errorf("%s: config field %s twice", typ, f.Key)
			}
			config[f.Key] = true
			check("config", f)
		}
		ids := map[string]bool{}
		for _, a := range m.Auth {
			if ids[a.ID] || a.ID == "" || a.Label == "" {
				t.Errorf("%s: auth method %q repeated or unnamed", typ, a.ID)
			}
			ids[a.ID] = true
			switch a.Flow {
			case destination.FlowFields:
				if len(a.Fields) == 0 {
					t.Errorf("%s/%s: a fields method without fields", typ, a.ID)
				}
			case destination.FlowGenerate:
				if _, ok := d.(destination.CredentialGenerator); !ok {
					t.Errorf("%s/%s: generates, but the driver can't", typ, a.ID)
				}
			case destination.FlowRedirect:
			default:
				t.Errorf("%s/%s: flow %q", typ, a.ID, a.Flow)
			}
			for _, f := range a.Fields {
				if config[f.Key] {
					t.Errorf("%s/%s: field %s is also a config field", typ, a.ID, f.Key)
				}
				check(a.ID, f)
			}
		}
		if audiences > 1 {
			t.Errorf("%s: %d audience fields", typ, audiences)
		}
	}
}

// What the form collects is what Open reads: each method's fields and the
// config, validated and split as the account API does, open an account
// (Open does no I/O, so the host refuses every connection).
func TestManifestFieldsOpenAnAccount(t *testing.T) {
	Register()
	samples := map[string]map[string]map[string]any{ // type → method → typed values
		"bluesky":  {"app_password": {"identifier": "ada.example", "app_password": "abcd-efgh-ijkl-mnop", "pds_url": "https://pds.example"}},
		"mastodon": {"token": {"access_token": "tok", "instance_url": "https://mastodon.example"}},
		"threads":  {"token": {"access_token": "tok"}},
		"nostr": {
			"secret_key": {"secret_key": "nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5", "bootstrap_relay": "wss://relay.example"},
			"generate":   {"bootstrap_relay": "wss://relay.example"},
		},
	}
	for _, typ := range destination.TypesInOrder() {
		d, _ := destination.Lookup(typ)
		m := d.Manifest()
		for _, a := range m.Auth {
			if a.Flow == destination.FlowRedirect {
				continue
			}
			in, ok := samples[typ][a.ID]
			if !ok {
				t.Errorf("%s/%s: no sample", typ, a.ID)
				continue
			}
			creds, cfgIn := map[string]any{}, map[string]any{}
			for k, v := range in {
				if _, isAuth := fieldOf(a.Fields, k); isAuth {
					creds[k] = v
				} else {
					cfgIn[k] = v
				}
			}
			cv, errs := destination.ValidateFields(a.Fields, creds, destination.FieldOptions{URL: ownerURL})
			if len(errs) != 0 {
				t.Errorf("%s/%s credentials: %+v", typ, a.ID, errs)
				continue
			}
			config, errs := destination.ValidateFields(m.Config, cfgIn, destination.FieldOptions{URL: ownerURL})
			if len(errs) != 0 {
				t.Errorf("%s/%s config: %+v", typ, a.ID, errs)
				continue
			}
			secrets, more := destination.SplitCredentials(a, cv)
			maps.Copy(config, more)
			if a.Flow == destination.FlowGenerate {
				if secrets, errs = generate(d, a.ID); errs != nil {
					t.Errorf("%s/%s: %v", typ, a.ID, errs)
					continue
				}
			}
			acct, err := d.Open(context.Background(), destination.OpenRequest{AccountID: "acc_t", Method: a.ID,
				Config: config, Secrets: secrets, Host: destinationtest.NewHost(t)})
			if err != nil {
				t.Errorf("%s/%s: Open: %v", typ, a.ID, err)
				continue
			}
			if c, ok := acct.(io.Closer); ok {
				_ = c.Close()
			}
		}
	}
}

func fieldOf(fs []destination.FieldSpec, key string) (destination.FieldSpec, bool) {
	for _, f := range fs {
		if f.Key == key {
			return f, true
		}
	}
	return destination.FieldSpec{}, false
}

func generate(d destination.Driver, method string) (map[string]string, []destination.FieldError) {
	g, ok := d.(destination.CredentialGenerator)
	if !ok {
		return nil, []destination.FieldError{{Detail: "not a generator"}}
	}
	s, err := g.GenerateCredentials(method)
	if err != nil {
		return nil, []destination.FieldError{{Detail: err.Error()}}
	}
	return s, nil
}

// A new Nostr account is never offered this instance's relay: the
// defaults are the operator's variables where set, else public relays.
func TestNewNostrAccountRelayDefaults(t *testing.T) {
	Register()
	d, _ := destination.Lookup(nostr.Type)
	m := d.Manifest()
	for _, f := range m.Config {
		if s := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(toString(f.Default))), "")); strings.Contains(s, "geoffrey") {
			t.Errorf("%s defaults to %v", f.Key, f.Default)
		}
	}
	none := func(string) string { return "" }
	if got := d.(destination.ConfigDefaulter).ConfigDefaults(none); len(got) != 0 {
		t.Errorf("defaults with no variables set: %v", got)
	}
	env := map[string]string{"NIP65_BOOTSTRAP_RELAY": "wss://home.example", "FALLBACK_RELAYS": "wss://a.example, wss://b.example",
		"POW_DIFFICULTY_DEFAULT": "0", "NOSTR_PRIMARY_FANOUT": "true"}
	got := d.(destination.ConfigDefaulter).ConfigDefaults(func(k string) string { return env[k] })
	if got[nostr.ConfigBootstrapRelay] != "wss://home.example" || strings.Join(got[nostr.ConfigFallbackRelays].([]string), " ") != "wss://a.example wss://b.example" ||
		got[nostr.ConfigPOWDefault] != 0 || got[nostr.ConfigPrimaryFanout] != true {
		t.Errorf("defaults from the environment: %v", got)
	}
	if _, ok := got[nostr.ConfigPOWMax]; ok {
		t.Errorf("an unset variable gave a default: %v", got)
	}
	// The operator's defaults validate like the built-in ones.
	if _, errs := destination.ValidateFields(destination.WithDefaults(m.Config, got), nil, destination.FieldOptions{URL: ownerURL}); len(errs) != 0 {
		t.Errorf("env defaults don't validate: %+v", errs)
	}
	for _, r := range nostr.NewAccountSyncRelays(none) {
		if strings.Contains(r, "geoffrey") {
			t.Errorf("sync seed offers %s", r)
		}
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []string:
		return strings.Join(x, " ")
	}
	return ""
}
