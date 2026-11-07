package destination

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func intp(n int) *int { return &n }

// testURL stands in for netguard's OwnerURL: lowercase, the secure scheme
// required, "bad" refused.
func testURL(raw string, r URLRule) (string, error) {
	if strings.Contains(raw, "bad") {
		return "", errors.New("that address is private or local")
	}
	if !strings.HasPrefix(strings.ToLower(raw), r.Secure+"://") {
		return "", errors.New("use " + r.Secure + "://")
	}
	return strings.ToLower(strings.TrimRight(raw, "/")), nil
}

var testSpecs = []FieldSpec{
	{Key: "name", Label: "Name", Type: FieldString, Required: true, MaxLength: 10},
	{Key: "notes", Label: "Notes", Type: FieldText},
	{Key: "on", Label: "On", Type: FieldBool, Default: true},
	{Key: "n", Label: "N", Type: FieldInt, Min: intp(0), Max: intp(5), Default: 2},
	{Key: "mode", Label: "Mode", Type: FieldEnum, Enum: []EnumValue{{Value: "a"}, {Value: "b"}}},
	{Key: "tags", Label: "Tags", Type: FieldStringList, Pattern: `[a-z]+`, MaxItems: 3},
	{Key: "site", Label: "Site", Type: FieldURL, URL: &HTTPSOrigin, Audience: true},
	{Key: "relays", Label: "Relays", Type: FieldURLList, URL: &RelayURL, Default: []string{"wss://default.example"}},
	{Key: "user", Label: "User", Type: FieldString, Pattern: `me|[0-9]+`, Default: "me"},
	{Key: "token", Label: "Token", Type: FieldString, Secret: true, Required: true},
}

func TestValidateFieldsNormalizes(t *testing.T) {
	var num json.Number = "4"
	got, errs := ValidateFields(testSpecs, map[string]any{
		"name": "  Ada ", "notes": "line one\nline two\ttab", "on": false, "n": num,
		"mode": "b", "tags": []any{"x", "y", "x", " "}, "site": "HTTPS://Example.COM/",
		"relays": []string{"wss://r.example", "WSS://R.example/"}, "token": " tok ",
	}, FieldOptions{URL: testURL})
	if len(errs) != 0 {
		t.Fatalf("errors: %+v", errs)
	}
	want := map[string]any{
		"name": "Ada", "notes": "line one\nline two\ttab", "on": false, "n": 4, "mode": "b",
		"tags": []string{"x", "y"}, "site": "https://example.com", "relays": []string{"wss://r.example"},
		"user": "me", "token": "tok",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
	// Defaults fill what is left out; an empty value is left out.
	got, errs = ValidateFields(testSpecs, map[string]any{"name": "x", "token": "t", "site": "", "n": nil}, FieldOptions{URL: testURL})
	if len(errs) != 0 || got["on"] != true || got["n"] != 2 || !reflect.DeepEqual(got["relays"], []string{"wss://default.example"}) {
		t.Errorf("defaults: %#v %+v", got, errs)
	}
	if _, ok := got["site"]; ok {
		t.Errorf("an empty site was kept: %#v", got)
	}
}

func TestValidateFieldsRefuses(t *testing.T) {
	secret := "s3cr3t-value"
	cases := map[string]struct {
		in   map[string]any
		code string
	}{
		"name":   {map[string]any{"name": strings.Repeat("é", 11)}, "too_long"},
		"notes":  {map[string]any{"notes": strings.Repeat("x", DefaultFieldMaxLength+1)}, "too_long"},
		"on":     {map[string]any{"on": "true"}, "invalid"},
		"n":      {map[string]any{"n": 6}, "out_of_range"},
		"mode":   {map[string]any{"mode": "c"}, "not_allowed"},
		"tags":   {map[string]any{"tags": []any{"a", "b", "c", "d"}}, "too_many"},
		"site":   {map[string]any{"site": "https://bad.example"}, "invalid"},
		"relays": {map[string]any{"relays": []any{"wss://ok.example", 3}}, "invalid"},
		"user":   {map[string]any{"user": "you"}, "invalid"},
		"token":  {map[string]any{"token": secret + "\x00"}, "invalid"},
		"extra":  {map[string]any{"extra": secret}, "unknown_field"},
	}
	for key, c := range cases {
		in := map[string]any{"name": "ok", "token": secret}
		for k, v := range c.in {
			in[k] = v
		}
		_, errs := ValidateFields(testSpecs, in, FieldOptions{URL: testURL})
		if len(errs) != 1 || errs[0].Key != key || errs[0].Code != c.code {
			t.Errorf("%s: errors %+v, want one %s", key, errs, c.code)
			continue
		}
		if strings.Contains(errs[0].Detail, secret) {
			t.Errorf("%s: the detail repeats the value: %s", key, errs[0].Detail)
		}
	}
	// A list item's pattern; a value of another type in a string field.
	if _, errs := ValidateFields(testSpecs, map[string]any{"name": "x", "token": "t", "tags": []any{"ok", "NO"}}, FieldOptions{URL: testURL}); len(errs) != 1 || !strings.Contains(errs[0].Detail, "value 2") {
		t.Errorf("list pattern: %+v", errs)
	}
	// A pattern matches the whole value.
	if _, errs := ValidateFields(testSpecs, map[string]any{"name": "x", "token": "t", "user": "meh"}, FieldOptions{URL: testURL}); len(errs) != 1 || errs[0].Key != "user" {
		t.Errorf("anchored pattern: %+v", errs)
	}
	if _, errs := ValidateFields(testSpecs, map[string]any{"name": 3, "token": "t"}, FieldOptions{URL: testURL}); len(errs) != 1 || errs[0].Code != "invalid" {
		t.Errorf("type: %+v", errs)
	}
	// Required fields left out, sorted by key; no URL check means no
	// address is accepted.
	_, errs := ValidateFields(testSpecs, map[string]any{"site": "https://x.example"}, FieldOptions{})
	if len(errs) != 3 || errs[0].Key != "name" || errs[1].Key != "site" || errs[2].Key != "token" || errs[0].Code != "required" {
		t.Errorf("required: %+v", errs)
	}
}

// An update checks only what it names; null (or empty) resets a field to
// its default, except a required one without a default.
func TestValidateFieldsPartial(t *testing.T) {
	got, errs := ValidateFields(testSpecs, map[string]any{"n": nil, "mode": "", "on": false}, FieldOptions{URL: testURL, Partial: true})
	if len(errs) != 0 || !reflect.DeepEqual(got, map[string]any{"n": nil, "mode": nil, "on": false}) {
		t.Errorf("partial: %#v %+v", got, errs)
	}
	for _, v := range []any{nil, " "} {
		if _, errs := ValidateFields(testSpecs, map[string]any{"name": v}, FieldOptions{Partial: true}); len(errs) != 1 || errs[0].Code != "required" {
			t.Errorf("clearing a required field (%q): %+v", v, errs)
		}
	}
}

func TestManifestFieldHelpers(t *testing.T) {
	m := Manifest{
		Auth: []AuthMethod{
			{ID: "pw", Flow: FlowFields, Fields: []FieldSpec{{Key: "handle", Type: FieldString}, {Key: "password", Type: FieldString, Secret: true}}},
			{ID: "key", Flow: FlowFields, Fields: []FieldSpec{{Key: "key", Type: FieldString, Secret: true, Revealable: true}}},
		},
		Config: []FieldSpec{{Key: "server", Type: FieldURL, URL: &HTTPSOrigin, Audience: true}, {Key: "n", Type: FieldInt, Default: 1}},
	}
	if a, ok := m.AuthMethod("key"); !ok || a.ID != "key" {
		t.Error("AuthMethod")
	}
	if _, ok := m.AuthMethod("nope"); ok {
		t.Error("AuthMethod found an unknown method")
	}
	if m.AudienceKey() != "server" {
		t.Errorf("AudienceKey = %q", m.AudienceKey())
	}
	if f, ok := m.SecretField("key"); !ok || !f.Revealable {
		t.Error("SecretField(key)")
	}
	if _, ok := m.SecretField("handle"); ok {
		t.Error("SecretField found a field that isn't secret")
	}
	secrets, config := SplitCredentials(m.Auth[0], map[string]any{"handle": "h", "password": "p"})
	if !reflect.DeepEqual(secrets, map[string]string{"password": "p"}) || !reflect.DeepEqual(config, map[string]any{"handle": "h"}) {
		t.Errorf("split: %v %v", secrets, config)
	}
	d := WithDefaults(m.Config, map[string]any{"n": 7})
	if d[1].Default != 7 || m.Config[1].Default != 1 {
		t.Errorf("WithDefaults: %v (original %v)", d[1].Default, m.Config[1].Default)
	}
}
