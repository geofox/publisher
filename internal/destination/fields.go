package destination

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// AuthMethod is one way to connect an account of a type: fields the owner
// types (an app password, a token, a key), a redirect to the network
// (OAuth, part E), or a credential Publisher generates (a Nostr key).
type AuthMethod struct {
	// ID is driver-defined ("app_password", "token"); it is stored with
	// the account (auth_method) and passed to Open as OpenRequest.Method.
	ID      string   `json:"id"`
	Flow    FlowKind `json:"flow"`
	Label   string   `json:"label"`
	Help    string   `json:"help,omitempty"`
	HelpURL string   `json:"help_url,omitempty"`
	// Fields are what the owner types. A Secret field becomes one of the
	// account's secrets; the others join its config (a Bluesky handle).
	Fields []FieldSpec `json:"fields,omitempty"`
	// NeedsInstanceConfig lists InstanceConfig keys that must be set
	// before this method can be used (a Threads app id).
	NeedsInstanceConfig []string `json:"needs_instance_config,omitempty"`
	// NeedsPublicURL: the method needs PUBLIC_URL (an OAuth redirect).
	NeedsPublicURL bool `json:"needs_public_url,omitempty"`
}

// FlowKind is how an AuthMethod collects the credential.
type FlowKind string

const (
	// FlowFields: the owner types the credential (Fields).
	FlowFields FlowKind = "fields"
	// FlowRedirect: the owner signs in at the network (part E).
	FlowRedirect FlowKind = "redirect"
	// FlowGenerate: Publisher makes the credential (Fields may hold the
	// rest, such as nothing at all).
	FlowGenerate FlowKind = "generate"
)

// FieldType is a field's value type.
type FieldType string

const (
	FieldString FieldType = "string" // one line of text
	FieldText   FieldType = "text"   // several lines
	FieldBool   FieldType = "bool"
	FieldInt    FieldType = "int"
	FieldEnum   FieldType = "enum" // one of Enum
	// FieldStringList is a list of one-line strings.
	FieldStringList FieldType = "string_list"
	// FieldURL is an address the owner types, checked and normalized by
	// the host under URL's rule (never loopback or link-local; a plain
	// scheme only for a host PRIVATE_DESTINATIONS lists).
	FieldURL FieldType = "url"
	// FieldURLList is a list of such addresses (relays).
	FieldURLList FieldType = "url_list"
)

// URLRule says what a url field takes: the Secure scheme for any host,
// the Plain one only for a private host the operator listed; Path keeps a
// path (a relay at wss://host/path), otherwise the value is an origin.
type URLRule struct {
	Secure string `json:"secure"`
	Plain  string `json:"plain,omitempty"`
	Path   bool   `json:"path,omitempty"`
}

// The rules the built-in drivers use.
var (
	// HTTPSOrigin is a server's origin over https (http for a listed
	// private host): a Mastodon instance, a PDS.
	HTTPSOrigin = URLRule{Secure: "https", Plain: "http"}
	// RelayURL is a WebSocket relay over wss (ws for a listed private
	// host), with a path when it has one.
	RelayURL = URLRule{Secure: "wss", Plain: "ws", Path: true}
)

// FieldSpec describes one field of an account's configuration or
// credentials, for the web app's form and for ValidateFields.
type FieldSpec struct {
	Key   string    `json:"key"`
	Label string    `json:"label"`
	Help  string    `json:"help,omitempty"`
	Type  FieldType `json:"type"`
	// Secret: a credential, sealed at rest and never shown again (only in
	// AuthMethod fields, never in Config).
	Secret bool `json:"secret,omitempty"`
	// Revealable: a Secret the owner may see again (:reveal), because it
	// is theirs to keep, such as a Nostr key Publisher generated.
	Revealable bool `json:"revealable,omitempty"`
	// Audience: the field says where the account's credentials are sent
	// (a Mastodon instance, a PDS); the credentials are bound to it, so
	// changing it needs them again. A url field; at most one per type.
	Audience bool `json:"audience,omitempty"`
	Required bool `json:"required,omitempty"`
	// Pattern is an RE2 expression the whole value must match (string
	// fields, each item of a string_list).
	Pattern     string      `json:"pattern,omitempty"`
	Placeholder string      `json:"placeholder,omitempty"`
	Enum        []EnumValue `json:"enum,omitempty"`
	// Default is the value a new account gets when the field is left out.
	Default any `json:"default,omitempty"`
	// Min and Max bound an int field.
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
	// MaxLength bounds a text value, in characters (default 4096).
	MaxLength int `json:"max_length,omitempty"`
	// MaxItems bounds a list (default 64).
	MaxItems int `json:"max_items,omitempty"`
	// URL is the rule of a url or url_list field.
	URL *URLRule `json:"url,omitempty"`
	// Advanced fields are folded away in the web app's form.
	Advanced bool `json:"advanced,omitempty"`
}

// Default bounds of a field.
const (
	DefaultFieldMaxLength = 4096
	DefaultFieldMaxItems  = 64
)

func (s FieldSpec) maxLength() int {
	if s.MaxLength > 0 {
		return s.MaxLength
	}
	return DefaultFieldMaxLength
}

func (s FieldSpec) maxItems() int {
	if s.MaxItems > 0 {
		return s.MaxItems
	}
	return DefaultFieldMaxItems
}

// AuthMethod returns the type's auth method with the given id.
func (m Manifest) AuthMethod(id string) (AuthMethod, bool) {
	for _, a := range m.Auth {
		if a.ID == id {
			return a, true
		}
	}
	return AuthMethod{}, false
}

// AudienceKey is the key of the type's Audience field ("" when its
// credentials only ever go to fixed hosts).
func (m Manifest) AudienceKey() string {
	for _, f := range m.Config {
		if f.Audience {
			return f.Key
		}
	}
	for _, a := range m.Auth {
		for _, f := range a.Fields {
			if f.Audience {
				return f.Key
			}
		}
	}
	return ""
}

// SecretField returns the auth field (of any method) holding the named
// secret.
func (m Manifest) SecretField(key string) (FieldSpec, bool) {
	for _, a := range m.Auth {
		for _, f := range a.Fields {
			if f.Secret && f.Key == key {
				return f, true
			}
		}
	}
	return FieldSpec{}, false
}

// WithDefaults returns specs with the defaults in d (by key) in place of
// their own: a type whose defaults depend on the server's configuration
// (ConfigDefaulter). specs is not modified.
func WithDefaults(specs []FieldSpec, d map[string]any) []FieldSpec {
	out := slices.Clone(specs)
	for i := range out {
		if v, ok := d[out[i].Key]; ok {
			out[i].Default = v
		}
	}
	return out
}

// ConfigDefaulter is implemented by drivers whose defaults for a new
// account depend on the server's configuration (Nostr offers the relays
// the operator set). The keys are Config keys.
type ConfigDefaulter interface {
	ConfigDefaults(getenv func(string) string) map[string]any
}

// CredentialGenerator is implemented by drivers with a FlowGenerate
// method: it makes the credential (a new Nostr key), keyed like the
// secrets Open reads.
type CredentialGenerator interface {
	GenerateCredentials(method string) (map[string]string, error)
}

// FieldError is one field's problem. Detail never repeats the value: the
// field may hold a credential.
type FieldError struct {
	Key    string `json:"key"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// FieldOptions is what ValidateFields needs from the host.
type FieldOptions struct {
	// URL checks and normalizes an address the owner typed under rule
	// (netguard.Policy.OwnerURL). nil refuses every url field.
	URL func(raw string, rule URLRule) (string, error)
	// Partial validates an update: only the fields given are checked, a
	// missing required field is not an error, no default is applied, and
	// null (nil) is kept, meaning "back to the default".
	Partial bool
}

// ValidateFields checks values (decoded from JSON, or Go values) against
// specs and returns them normalized: strings trimmed, numbers as int,
// lists as []string, addresses as the host's URL check returns them, and
// (unless o.Partial) the defaults of fields left out. An unknown key, a
// missing required field, a value of the wrong type, out of bounds, not
// matching its pattern or not one of its choices is a FieldError; errors
// are sorted by key.
func ValidateFields(specs []FieldSpec, values map[string]any, o FieldOptions) (map[string]any, []FieldError) {
	byKey := make(map[string]FieldSpec, len(specs))
	for _, s := range specs {
		byKey[s.Key] = s
	}
	out := make(map[string]any, len(specs))
	var errs []FieldError
	bad := func(key, code, detail string) {
		errs = append(errs, FieldError{Key: key, Code: code, Detail: detail})
	}
	for k, v := range values {
		s, ok := byKey[k]
		if !ok {
			bad(k, "unknown_field", "not a field of this account type")
			continue
		}
		var nv any
		if v != nil {
			var code, detail string
			if nv, code, detail = s.normalize(v, o); code != "" {
				bad(k, code, detail)
				continue
			}
		}
		switch {
		case !isEmpty(nv):
			out[k] = nv
		case !o.Partial:
			// An empty value is a field left out.
		case s.Required && s.Default == nil:
			bad(k, "required", s.Label+" is required")
		default:
			out[k] = nil // back to the default
		}
	}
	if !o.Partial {
		for _, s := range specs {
			if _, ok := out[s.Key]; ok || hasErr(errs, s.Key) {
				continue
			}
			if s.Default != nil {
				if nv, code, _ := s.normalize(s.Default, o); code == "" && !isEmpty(nv) {
					out[s.Key] = nv
					continue
				}
			}
			if s.Required {
				bad(s.Key, "required", s.Label+" is required")
			}
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Key < errs[j].Key })
	return out, errs
}

func hasErr(errs []FieldError, key string) bool {
	return slices.ContainsFunc(errs, func(e FieldError) bool { return e.Key == key })
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []string:
		return len(x) == 0
	}
	return false
}

// normalize checks one non-nil value; code is "" when it is good.
func (s FieldSpec) normalize(v any, o FieldOptions) (out any, code, detail string) {
	switch s.Type {
	case FieldBool:
		b, ok := v.(bool)
		if !ok {
			return nil, "invalid", "must be true or false"
		}
		return b, "", ""
	case FieldInt:
		n, ok := wholeNumber(v)
		if !ok {
			return nil, "invalid", "must be a whole number"
		}
		if s.Min != nil && n < *s.Min || s.Max != nil && n > *s.Max {
			return nil, "out_of_range", s.bounds()
		}
		return n, "", ""
	case FieldString, FieldText, FieldEnum, FieldURL:
		str, ok := v.(string)
		if !ok {
			return nil, "invalid", "must be text"
		}
		return s.oneString(str, o)
	case FieldStringList, FieldURLList:
		items, ok := stringItems(v)
		if !ok {
			return nil, "invalid", "must be a list of text values"
		}
		// Repeats and blanks are dropped before the count; a list far
		// longer than allowed isn't read at all.
		tooMany := fmt.Sprintf("at most %d values", s.maxItems())
		if len(items) > 4*s.maxItems() {
			return nil, "too_many", tooMany
		}
		list := make([]string, 0, len(items))
		for i, it := range items {
			nv, code, detail := s.oneString(it, o)
			if code != "" {
				return nil, code, fmt.Sprintf("value %d: %s", i+1, detail)
			}
			if str := nv.(string); str != "" && !slices.Contains(list, str) {
				list = append(list, str)
			}
		}
		if len(list) > s.maxItems() {
			return nil, "too_many", tooMany
		}
		return list, "", ""
	}
	return nil, "invalid", "this field can't be set"
}

// oneString checks a string value, or one item of a list.
func (s FieldSpec) oneString(str string, o FieldOptions) (any, string, string) {
	str = strings.TrimSpace(str)
	if str == "" {
		return "", "", ""
	}
	if utf8.RuneCountInString(str) > s.maxLength() {
		return nil, "too_long", fmt.Sprintf("at most %d characters", s.maxLength())
	}
	if !utf8.ValidString(str) || strings.IndexFunc(str, func(r rune) bool {
		return unicode.IsControl(r) && !(s.Type == FieldText && (r == '\n' || r == '\t' || r == '\r'))
	}) >= 0 {
		return nil, "invalid", "contains characters that aren't allowed"
	}
	switch s.Type {
	case FieldEnum:
		if !slices.ContainsFunc(s.Enum, func(e EnumValue) bool { return e.Value == str }) {
			return nil, "not_allowed", "must be one of " + enumChoices(s.Enum)
		}
	case FieldURL, FieldURLList:
		if o.URL == nil || s.URL == nil {
			return nil, "invalid", "addresses can't be checked here"
		}
		u, err := o.URL(str, *s.URL)
		if err != nil {
			return nil, "invalid", err.Error()
		}
		return u, "", ""
	}
	if s.Pattern != "" {
		re, err := regexp.Compile(`^(?:` + s.Pattern + `)$`)
		if err != nil || !re.MatchString(str) {
			return nil, "invalid", "isn't in the expected form"
		}
	}
	return str, "", ""
}

func (s FieldSpec) bounds() string {
	switch {
	case s.Min != nil && s.Max != nil:
		return fmt.Sprintf("must be from %d to %d", *s.Min, *s.Max)
	case s.Min != nil:
		return fmt.Sprintf("must be at least %d", *s.Min)
	default:
		return fmt.Sprintf("must be at most %d", *s.Max)
	}
}

func enumChoices(e []EnumValue) string {
	vals := make([]string, 0, len(e))
	for _, x := range e {
		vals = append(vals, fmt.Sprintf("%q", x.Value))
	}
	return strings.Join(vals, ", ")
}

// wholeNumber reads an int from a Go or JSON-decoded number.
func wholeNumber(v any) (int, bool) {
	var f float64
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		f = float64(x)
	case float64:
		f = x
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if f != math.Trunc(f) || math.Abs(f) > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}

// stringItems reads a list of strings ([]string or a JSON []any).
func stringItems(v any) ([]string, bool) {
	switch x := v.(type) {
	case []string:
		return x, true
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// SplitCredentials splits a method's validated fields into the account's
// secrets (Secret fields) and the config the others join.
func SplitCredentials(m AuthMethod, values map[string]any) (secrets map[string]string, config map[string]any) {
	secrets, config = map[string]string{}, map[string]any{}
	for _, f := range m.Fields {
		v, ok := values[f.Key]
		if !ok {
			continue
		}
		if f.Secret {
			if s, ok := v.(string); ok {
				secrets[f.Key] = s
			}
			continue
		}
		config[f.Key] = v
	}
	return secrets, config
}
