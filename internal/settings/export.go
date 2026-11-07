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

// Skipped is a setting an import leaves as it is, and why.
type Skipped struct {
	Key string `json:"key"`
	Why string `json:"why"`
}

// PlanImport splits an export's values for a scope: the keys it would
// save, and those it leaves (unknown here, or set by the environment
// here: the operator's word wins).
func (s *Service) PlanImport(scope Scope, values map[string]any) (set []string, skipped []Skipped) {
	cur := s.Current()
	for k := range values {
		d, ok := s.by[k]
		switch {
		case !ok || d.Scope != scope:
			skipped = append(skipped, Skipped{Key: k, Why: "not a setting on this server"})
		case cur.vals[k].Locked:
			skipped = append(skipped, Skipped{Key: k, Why: "set by " + s.envName(d) + " in this server's environment"})
		default:
			set = append(set, k)
		}
	}
	sort.Strings(set)
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Key < skipped[j].Key })
	return set, skipped
}

// Import saves an export's values for a scope, as PlanImport split them.
// A value this server refuses (an address it can't reach, say) is left
// out, with what goes with it (the secret sent to that address), and the
// rest is saved.
func (s *Service) Import(ctx context.Context, scope Scope, values map[string]any, by string) ([]string, []Skipped, error) {
	set, skipped := s.PlanImport(scope, values)
	vals := map[string]any{}
	for _, k := range set {
		vals[k] = values[k]
	}
	for len(vals) > 0 {
		_, err := s.Apply(ctx, Change{Scope: scope, Values: vals, IfMatch: "*", By: by})
		var ve *ValidationError
		var le *LockedError
		switch {
		case err == nil:
			saved := make([]string, 0, len(vals))
			for k := range vals {
				saved = append(saved, k)
			}
			sort.Strings(saved)
			return saved, skipped, nil
		case errors.As(err, &le):
			// Set by the environment since the plan.
			skipped = append(skipped, Skipped{Key: le.Key, Why: "set by the server's environment"})
			delete(vals, le.Key)
		case errors.As(err, &ve) && len(ve.Fields) > 0:
			before := len(vals)
			for _, f := range ve.Fields {
				if _, ok := vals[f.Key]; ok {
					skipped = append(skipped, Skipped{Key: f.Key, Why: f.Detail})
					delete(vals, f.Key)
				}
				for _, d := range s.defs {
					if d.AudienceOf == f.Key {
						if _, ok := vals[d.Key]; ok {
							skipped = append(skipped, Skipped{Key: d.Key, Why: "goes with " + f.Key + ", which wasn't saved"})
							delete(vals, d.Key)
						}
					}
				}
			}
			if len(vals) == before {
				// A rule across keys (Blossom chosen without its address):
				// the choice is left as it is.
				if _, ok := vals[MediaStore]; ok {
					skipped = append(skipped, Skipped{Key: MediaStore, Why: ve.Fields[0].Detail})
					delete(vals, MediaStore)
					continue
				}
				return nil, skipped, err
			}
		default:
			return nil, skipped, err
		}
	}
	return nil, skipped, nil
}
