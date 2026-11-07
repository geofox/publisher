package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/store"
)

// An account's defaults (spec §7.4) are what a post to it gets when the
// post says nothing of them: a content warning, languages, and the type's
// options (visibility among them, as in a target's options). A target's
// own value comes first, then the post's shared content, then the
// account's default; an explicit "" or [] (or an option's "") clears a
// default for that post.

// v1AccountDefaults is an account's defaults, as read and written.
type v1AccountDefaults struct {
	ContentWarning string         `json:"content_warning,omitempty"`
	Languages      []string       `json:"languages,omitempty"`
	Options        map[string]any `json:"options,omitempty"`
}

func (d v1AccountDefaults) empty() bool {
	return d.ContentWarning == "" && len(d.Languages) == 0 && len(d.Options) == 0
}

// accountDefaults reads an account's saved defaults ({} when none, or
// when what is saved can't be read).
func accountDefaults(ac store.Account) v1AccountDefaults {
	var d v1AccountDefaults
	if strings.TrimSpace(ac.Defaults) == "" {
		return d
	}
	if err := json.Unmarshal([]byte(ac.Defaults), &d); err != nil {
		return v1AccountDefaults{}
	}
	return d
}

// checkAccountDefaults validates a PATCH's defaults against the account's
// capabilities and its type's options, and answers them as saved: blanks
// and nulls dropped, so {} clears them all.
func (a *API) checkAccountDefaults(ctx context.Context, ac store.Account, raw json.RawMessage) (string, []fieldError) {
	var errs []fieldError
	add := func(ptr, code, detail string) {
		errs = append(errs, fieldError{Pointer: ptr, Code: code, Detail: detail})
	}
	var in v1AccountDefaults
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if isJSONNull(raw) || dec.Decode(&in) != nil {
		add("/defaults", "invalid", "an object of content_warning, languages and options ({} clears them)")
		return "", errs
	}
	drv, ok := destination.Lookup(ac.Type)
	if !ok {
		add("/defaults", "invalid", "this server has no driver for "+ac.Type)
		return "", errs
	}
	m := drv.Manifest()
	caps := a.caps(ctx, ac.ID, ac.Type)
	out := v1AccountDefaults{}
	if cw := strings.TrimSpace(in.ContentWarning); cw != "" {
		switch {
		case utf8.RuneCountInString(cw) > maxCWRunes:
			add("/defaults/content_warning", "too_long", "at most 1000 characters")
		case !caps.ContentWarning:
			add("/defaults/content_warning", "unsupported", m.Name+" has no content warnings")
		default:
			out.ContentWarning = cw
		}
	}
	if len(in.Languages) > 0 {
		switch {
		case caps.Languages == 0:
			add("/defaults/languages", "unsupported", m.Name+" takes no languages")
		case len(in.Languages) > caps.Languages:
			add("/defaults/languages", "too_many", m.Name+" takes "+strconv.Itoa(caps.Languages)+" language(s)")
		default:
			for i, l := range in.Languages {
				if !langTag.MatchString(l) {
					add("/defaults/languages/"+strconv.Itoa(i), "invalid_language", "a language tag such as en or pt-BR")
				}
			}
			out.Languages = in.Languages
		}
	}
	var ov dispatch.Overrides
	n := len(errs)
	a.planOptions(m, caps, in.Options, "/defaults/options", &ov, add)
	if len(errs) == n {
		for k, v := range in.Options {
			if v == nil || v == "" {
				continue // the destination's own default
			}
			if out.Options == nil {
				out.Options = map[string]any{}
			}
			out.Options[k] = v
		}
	}
	if len(errs) > 0 {
		return "", errs
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", []fieldError{{Pointer: "/defaults", Code: "invalid", Detail: err.Error()}}
	}
	return string(b), nil
}

// applyDefaults fills in what a target left to the account: the content
// warning and languages when neither the target nor the shared content
// set them, and each option the target didn't name (or named null). A
// default the account can no longer take is left out with a warning. It
// returns the options to plan and what it applied.
func (a *API) applyDefaults(m destination.Manifest, caps destination.Capabilities, d v1AccountDefaults,
	cw **string, langs *[]string, opts map[string]any, pt *plannedTarget) (map[string]any, v1AccountDefaults) {
	var applied v1AccountDefaults
	if d.empty() {
		return opts, applied
	}
	if *cw == nil && d.ContentWarning != "" {
		if caps.ContentWarning {
			v := d.ContentWarning
			*cw, applied.ContentWarning = &v, v
		} else {
			pt.warnings = append(pt.warnings, "the account's default content warning is left out: "+m.Name+" no longer takes one")
		}
	}
	if *langs == nil && len(d.Languages) > 0 {
		switch {
		case caps.Languages == 0:
			pt.warnings = append(pt.warnings, "the account's default languages are left out: "+m.Name+" no longer takes languages")
		default:
			l := d.Languages[:min(len(d.Languages), caps.Languages)]
			*langs, applied.Languages = slices.Clone(l), slices.Clone(l)
		}
	}
	keys := make([]string, 0, len(d.Options))
	for k := range d.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	merged, copied := opts, false
	for _, k := range keys {
		if v, named := opts[k]; named && v != nil {
			continue // the target's own value
		}
		// A default the account can't take now (its server changed) is
		// a warning, not a refused post.
		var ov dispatch.Overrides
		bad := ""
		a.planOptions(m, caps, map[string]any{k: d.Options[k]}, "", &ov, func(_, _, detail string) { bad = detail })
		if bad != "" {
			pt.warnings = append(pt.warnings, fmt.Sprintf("the account's default %s is left out: %s", k, bad))
			continue
		}
		if !copied { // the request's map stays as sent
			merged, copied = make(map[string]any, len(opts)+len(keys)), true
			for ok, ov := range opts {
				merged[ok] = ov
			}
		}
		merged[k] = d.Options[k]
		if applied.Options == nil {
			applied.Options = map[string]any{}
		}
		applied.Options[k] = d.Options[k]
	}
	for _, s := range appliedNotes(applied) {
		pt.warnings = append(pt.warnings, "default applied: "+s)
	}
	return merged, applied
}

// appliedNotes says what defaults were applied ("visibility=unlisted").
func appliedNotes(d v1AccountDefaults) []string {
	var out []string
	if d.ContentWarning != "" {
		out = append(out, "content_warning="+d.ContentWarning)
	}
	if len(d.Languages) > 0 {
		out = append(out, "languages="+strings.Join(d.Languages, ","))
	}
	keys := make([]string, 0, len(d.Options))
	for k := range d.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%v", k, d.Options[k]))
	}
	return out
}
