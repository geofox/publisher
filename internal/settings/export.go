package settings

import (
	"context"
	"errors"
	"sort"
)

// Saved is what the app saved in a scope, secrets opened: for an export.
// The environment's values and the defaults aren't in it, nor a saved
// secret that can't be opened, nor a value the environment hides.
func (s *Snapshot) Saved(scope Scope) map[string]any {
	out := map[string]any{}
	for k, v := range s.vals {
		if v.def.Scope != scope || v.Source != SourceSaved {
			continue
		}
		if v.def.Field.Secret {
			out[k] = s.secret[k]
		} else {
			out[k] = v.Value
		}
	}
	return out
}

// Item is a setting in an import's plan or outcome, and why when it is
// left as it is (or, for a secret cleared, why it goes).
type Item struct {
	Key string `json:"key"`
	Why string `json:"why,omitempty"`
}

// PlanImport is what Import would do with an export's values for a
// scope, without saving: the same checks, run as a dry run.
func (s *Service) PlanImport(ctx context.Context, scope Scope, values map[string]any) (set, skipped []Item) {
	set, skipped, _ = s.importValues(ctx, scope, values, "", true)
	return set, skipped
}

// Import saves an export's values for a scope. What this server's
// environment sets stays (the operator's word wins), and so does what
// isn't a setting here. A secret sent to an address (a webhook's
// password) comes only with its address from the file, and an address
// the file replaces takes the secret saved for the old one along. A value
// this server refuses (an address it can't reach, say) is left out, with
// what goes with it, and the rest is saved. The error is only for a
// failure to save.
func (s *Service) Import(ctx context.Context, scope Scope, values map[string]any, by string) (saved, skipped []Item, err error) {
	return s.importValues(ctx, scope, values, by, false)
}

const (
	whyNotHere     = "not a setting on this server"
	whyMediaPublic = "turned on in Settings → Media here, once the test there passes"
)

func (s *Service) importValues(ctx context.Context, scope Scope, values map[string]any, by string, dry bool) ([]Item, []Item, error) {
	cur := s.Current()
	var skipped []Item
	skip := func(k, why string) { skipped = append(skipped, Item{Key: k, Why: why}) }
	vals := map[string]any{}
	for k, v := range values {
		d, ok := s.by[k]
		switch {
		case !ok || d.Scope != scope:
			skip(k, whyNotHere)
		case cur.vals[k].Locked:
			skip(k, "set by "+s.envName(d)+" in this server's environment")
		case k == MediaPublic && v == true:
			// Serving media publicly is turned on only once a probe
			// fetched them back at this server's address.
			skip(k, whyMediaPublic)
		default:
			vals[k] = v
		}
	}
	why := map[string]string{} // a secret cleared: why
	for _, d := range s.defs {
		if d.Scope != scope || d.AudienceOf == "" {
			continue
		}
		_, secret := vals[d.Key]
		addr, hasAddr := vals[d.AudienceOf]
		switch {
		case secret && !hasAddr:
			// Sealed here, it would go to whatever address this server
			// has: the file's secret goes only with the file's address.
			delete(vals, d.Key)
			skip(d.Key, "goes with "+d.AudienceOf+", which isn't imported here")
		case !secret && hasAddr && cur.vals[d.Key].Set && !cur.vals[d.Key].Locked && cur.vals[d.Key].Source == SourceSaved:
			if a, _ := addr.(string); Origin(a) != Origin(cur.secret[d.AudienceOf]) {
				vals[d.Key] = nil
				why[d.Key] = "cleared: it went with the address the file replaces"
			}
		}
	}
	dropWith := func(k, reason string) {
		if _, ok := vals[k]; ok {
			skip(k, reason)
			delete(vals, k)
		}
		for _, d := range s.defs {
			if d.AudienceOf == k {
				if _, ok := vals[d.Key]; ok {
					skip(d.Key, "goes with "+k+", which wasn't saved")
					delete(vals, d.Key)
				}
			}
		}
	}
	finish := func() []Item {
		set := make([]Item, 0, len(vals))
		for k := range vals {
			set = append(set, Item{Key: k, Why: why[k]})
		}
		sort.Slice(set, func(i, j int) bool { return set[i].Key < set[j].Key })
		sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].Key < skipped[j].Key })
		return set
	}
	for len(vals) > 0 {
		_, err := s.Apply(ctx, Change{Scope: scope, Values: vals, IfMatch: "*", By: by, DryRun: dry})
		var ve *ValidationError
		var le *LockedError
		switch {
		case err == nil:
			return finish(), skipped, nil
		case errors.As(err, &le):
			// Set by the environment since the plan.
			dropWith(le.Key, "set by the server's environment")
		case errors.As(err, &ve) && len(ve.Fields) > 0:
			before := len(vals)
			for _, f := range ve.Fields {
				if _, ok := vals[f.Key]; ok {
					dropWith(f.Key, f.Detail)
				} else if d, ok := s.by[f.Key]; ok && d.AudienceOf != "" {
					// A secret here that the change would orphan: the
					// address that needs it stays as it is.
					dropWith(d.AudienceOf, f.Detail)
				}
			}
			if len(vals) == before {
				// A rule across keys (Blossom chosen without its
				// address): the choice is left as it is, and anything
				// else this server can't take with it.
				if _, ok := vals[MediaStore]; ok {
					dropWith(MediaStore, ve.Fields[0].Detail)
					continue
				}
				for k := range vals {
					skip(k, ve.Fields[0].Detail)
				}
				vals = map[string]any{}
			}
		default:
			return nil, skipped, err
		}
	}
	return finish(), skipped, nil
}
