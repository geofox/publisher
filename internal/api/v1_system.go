package api

import (
	"net/http"
	"net/url"
	"strings"
)

// SystemValue is a bootstrap or tuning variable as Settings, System shows
// it (spec §7.5): its value as the environment gives it, or for a secret
// only whether it is set.
type SystemValue struct {
	Env    string `json:"env"`
	Set    bool   `json:"set"`
	Secret bool   `json:"secret,omitempty"`
	// Value is the variable's value; never a secret's, and an address
	// without the user and password it may carry.
	Value string `json:"value,omitempty"`
}

// SystemVar names a variable for System: Secret ones are shown as set or
// not, never their value.
type SystemVar struct {
	Env    string
	Secret bool
}

// SystemValues reads vars from getenv for System.
func SystemValues(getenv func(string) string, vars []SystemVar) []SystemValue {
	out := make([]SystemValue, 0, len(vars))
	for _, v := range vars {
		raw := strings.TrimSpace(getenv(v.Env))
		sv := SystemValue{Env: v.Env, Set: raw != "", Secret: v.Secret}
		if sv.Set && !v.Secret {
			sv.Value = withoutUserinfo(raw)
		}
		out = append(out, sv)
	}
	return out
}

// withoutUserinfo drops a user and password from each address in a value
// (a list is comma-separated).
func withoutUserinfo(raw string) string {
	parts := strings.Split(raw, ",")
	for i, p := range parts {
		if u, err := url.Parse(strings.TrimSpace(p)); err == nil && u.User != nil && u.Host != "" {
			u.User = nil
			parts[i] = u.String()
		} else if err != nil && strings.Contains(p, "@") && strings.Contains(p, "://") {
			parts[i] = "(an address that can't be shown)"
		}
	}
	return strings.Join(parts, ",")
}

type v1SystemBody struct {
	Version string        `json:"version"`
	Auth    string        `json:"auth"`
	Values  []SystemValue `json:"values"`
}

// v1System answers GET /v1/instance/system: the version, how the owner
// signs in, and the variables the server started with.
func (a *API) v1System(w http.ResponseWriter, r *http.Request) {
	body := v1SystemBody{Version: a.Version, Auth: a.mode(), Values: []SystemValue{}}
	if a.System != nil {
		body.Values = a.System()
	}
	writeV1(w, http.StatusOK, body)
}
