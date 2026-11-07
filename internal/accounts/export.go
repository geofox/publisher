package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/export"
	"github.com/geofox/publisher/internal/store"
)

// ExportAccounts is an owner's accounts as an export file holds them
// (spec §9): those added in the app with their configuration and every
// credential, opened; those from the environment with only what the owner
// chose for them (the environment holds the rest).
func (r *Registry) ExportAccounts(ctx context.Context, owner string) ([]export.Account, error) {
	var out []export.Account
	for _, e := range r.List(owner) {
		row := e.Row
		x := export.Account{ID: row.ID, Type: row.Type, Label: row.Label, Handle: row.Handle, RemoteID: row.RemoteID,
			Enabled: row.Enabled, DefaultSelected: row.DefaultSelected, PublicFeed: row.PublicFeed}
		if row.Defaults != "" && row.Defaults != "{}" {
			x.Defaults = json.RawMessage(row.Defaults)
		}
		if row.Source != store.SourceDB {
			x.Source, x.EnvKey = export.SourceEnv, row.EnvKey
			out = append(out, x)
			continue
		}
		d, ok := destination.Lookup(row.Type)
		if !ok {
			continue // a type this build doesn't have: nothing here can open it
		}
		cfg, err := rowConfig(row)
		if err != nil {
			return nil, err
		}
		sec, _, err := r.openSecrets(ctx, row, destination.AudienceOf(d.Manifest(), cfg))
		if err != nil {
			return nil, err
		}
		x.Source, x.Method, x.Config, x.Secrets = export.SourceApp, row.AuthMethod, cfg, sec
		out = append(out, x)
	}
	return out, nil
}

// Import outcomes (Imported.Status).
const (
	// ImportConnected: added, and its network said who it is.
	ImportConnected = "connected"
	// ImportRefused: added, but its network refused its credentials:
	// it needs reconnecting.
	ImportRefused = "refused"
	// ImportUnchecked: added, but its network didn't answer; the next
	// check says whether it works.
	ImportUnchecked = "unchecked"
	// ImportSkipped: not added (Detail says why).
	ImportSkipped = "skipped"
)

// Imported is what importing one account did.
type Imported struct {
	// ID is its id here ("" when it wasn't added).
	ID     string
	Status string
	Detail string
	// Handle is who it is: as its network said, else as the file said.
	Handle string
}

// ImportAccount adds an account from an export file (one added in the
// app where it was exported). It is checked as a new one is: its type and
// way of connecting must be offered here, its settings allowed here (an
// address this server may reach), and it is opened and identified before
// it is stored. Credentials its network refuses, or a network that
// doesn't answer, still add it, as needing a look: what the owner set for
// it stays. An account already here (the same network identity: from the
// environment, say) is skipped.
func (r *Registry) ImportAccount(ctx context.Context, owner string, x export.Account, actor string) (Imported, error) {
	skip := func(why string) (Imported, error) {
		return Imported{Status: ImportSkipped, Detail: why, Handle: x.Handle}, nil
	}
	if r.opt.Writes == nil {
		return Imported{}, errNoWriter
	}
	if err := r.opt.KeyState.WriteError(); err != nil {
		return Imported{}, err
	}
	d, a, config, why := r.checkImport(owner, x)
	if why != "" {
		return skip(why)
	}
	secrets := maps.Clone(x.Secrets)
	id := store.NewAccountID()
	row := store.Account{ID: id, OwnerID: owner, Type: x.Type, Source: store.SourceDB, AuthMethod: a.ID}
	out := Imported{Status: ImportConnected, Handle: x.Handle}
	ident, s2, c2, err := r.trial(ctx, d, row, config, secrets)
	var rej *RejectedError
	var up *UpstreamError
	switch {
	case err == nil:
		secrets, config = s2, c2
		if x.RemoteID != "" && ident.RemoteID != "" && ident.RemoteID != x.RemoteID {
			out.Detail = "its credentials now sign in as " + ident.Handle
		}
		out.Handle = ident.Handle
	case errors.As(err, &rej):
		out.Status, out.Detail = ImportRefused, rej.Detail
		ident = destination.Identity{Handle: x.Handle, RemoteID: x.RemoteID}
	case errors.As(err, &up):
		out.Status, out.Detail = ImportUnchecked, up.Detail
		ident = destination.Identity{Handle: x.Handle, RemoteID: x.RemoteID}
	default:
		var ie *InvalidError
		if errors.As(err, &ie) && len(ie.Problems) > 0 {
			return skip(ie.Problems[0].Detail)
		}
		return Imported{}, err
	}
	n := NewAccount{Owner: owner, Type: x.Type, Method: a.ID, Label: x.Label, Actor: actor}
	if _, err := r.storeNew(ctx, n, d, a, id, ident, config, secrets); err != nil {
		var dup *store.DuplicateAccountError
		if errors.As(err, &dup) {
			return skip("already here, as " + dup.Label)
		}
		return Imported{}, err
	}
	out.ID = id
	return out, nil
}

// CheckImport says why an account in an export file wouldn't be added
// here ("" when it would be, if its network takes its credentials): the
// checks ImportAccount makes before asking the network.
func (r *Registry) CheckImport(owner string, x export.Account) string {
	_, _, _, why := r.checkImport(owner, x)
	return why
}

// checkImport checks an account in a file as a new one is checked: its
// type and way of connecting offered here, what the owner set allowed
// here (an address this server may reach), every required credential
// there, and not here already. It answers the driver, the method and the
// configuration to open it with, or why it is skipped.
func (r *Registry) checkImport(owner string, x export.Account) (destination.Driver, destination.AuthMethod, map[string]any, string) {
	var none destination.AuthMethod
	if x.Source != export.SourceApp {
		return nil, none, nil, "not an account added in the app"
	}
	d, ok := destination.Lookup(x.Type)
	if !ok {
		return nil, none, nil, "this server has no accounts of that network"
	}
	m := d.Manifest()
	a, ok := m.AuthMethod(x.Method)
	if !ok {
		return nil, none, nil, "its way of connecting isn't offered here"
	}
	if e, dup := r.liveDuplicate(owner, x.Type, x.RemoteID); dup {
		return nil, none, nil, alreadyHere(e)
	}
	// What the owner set is checked as when it was added (an address
	// this server may reach); the rest is state its driver saved.
	keys := specKeys(m, a)
	owned, state := map[string]any{}, map[string]any{}
	for k, v := range x.Config {
		if keys[k] {
			owned[k] = v
		} else {
			state[k] = v
		}
	}
	specs := r.configSpecs(d)
	for _, f := range a.Fields {
		if !f.Secret {
			specs = append(specs, f)
		}
	}
	config, errs := destination.ValidateFields(specs, owned, r.fieldOptions(false))
	if len(errs) > 0 {
		return nil, none, nil, errs[0].Key + ": " + errs[0].Detail
	}
	maps.Copy(config, state)
	for _, f := range a.Fields {
		if f.Secret && f.Required && x.Secrets[f.Key] == "" {
			return nil, none, nil, "its credentials aren't all in the file"
		}
	}
	return d, a, config, ""
}

// liveDuplicate is the owner's account of typ with remoteID, if any.
func (r *Registry) liveDuplicate(owner, typ, remoteID string) (Entry, bool) {
	if remoteID == "" {
		return Entry{}, false
	}
	for _, e := range r.OfType(owner, typ) {
		if e.Row.RemoteID == remoteID {
			return e, true
		}
	}
	return Entry{}, false
}

func alreadyHere(e Entry) string {
	if e.Row.Source != store.SourceDB {
		return "this server's environment sets it (" + e.Row.Label + ")"
	}
	return "already here, as " + e.Row.Label
}

// EnvMatch is the owner's account from the environment that an export's
// environment account is: the same network identity, else (for one not
// yet identified) the same type and variables.
func (r *Registry) EnvMatch(owner string, x export.Account) (store.Account, bool) {
	var byKey *store.Account
	for _, e := range r.OfType(owner, x.Type) {
		row := e.Row
		if row.Source == store.SourceDB {
			continue
		}
		if x.RemoteID != "" && row.RemoteID == x.RemoteID {
			return row, true
		}
		if (x.RemoteID == "" || row.RemoteID == "") && x.EnvKey != "" && row.EnvKey == x.EnvKey && byKey == nil {
			byKey = &row
		}
	}
	if byKey != nil {
		return *byKey, true
	}
	return store.Account{}, false
}
