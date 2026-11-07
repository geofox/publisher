package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/geofox/publisher/internal/accounts"
	"github.com/geofox/publisher/internal/drivers"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// prepareSecrets runs before the registry loads: the rotation and status
// of the master key (accounts.PrepareKeys), then where accounts from the
// environment keep their saved state. With a usable key that is the
// secret store, and 2.x's plaintext Threads token moves there; while the
// token is still in its old row (no key, a key that doesn't match, or the
// move failed) it stays there and is used from there. An error means the
// store couldn't be read.
func prepareSecrets(ctx context.Context, st *store.Store, keys secrets.Keyring, envRows []store.Account, env []drivers.EnvAccount, dbPath string) (accounts.KeyState, accounts.StateStore, error) {
	ks, err := accounts.PrepareKeys(ctx, st, keys)
	if err != nil {
		return ks, nil, err
	}
	if ks.Rotated > 0 {
		slog.Info("master key: saved credentials moved to the new key; remove PUBLISHER_MASTER_KEY_PREVIOUS now", "count", ks.Rotated)
	}
	switch ks.Status {
	case accounts.KeyMissing, accounts.KeyMismatch:
		slog.Error("master key: " + ks.Detail)
	}
	legacy := drivers.LegacyState{Store: st}
	if !keys.HasKey() {
		return ks, legacy, nil
	}
	sealed := &accounts.SealedState{Store: st, Keys: keys, KeyState: ks}
	if ks.Status != accounts.KeyOK {
		if tok, err := st.GetThreadsToken(); err != nil || tok != nil {
			return ks, legacy, err
		}
		return ks, sealed, nil
	}
	to, done, err := drivers.MoveThreadsToken(ctx, st, sealed.Prepare, envRows, env)
	switch {
	case !done:
		if err != nil {
			slog.Error("moving the Threads token into the secret store failed; it stays where it was and the move is tried again at the next start", "err", err)
		}
		if tok, rerr := st.GetThreadsToken(); rerr != nil || tok != nil {
			return ks, legacy, rerr
		}
	case err != nil:
		slog.Warn("master key: "+err.Error(), "account", to)
	}
	if done && to != "" {
		slog.Info("master key: the Threads token is now stored encrypted", "account", to)
		if baks, _ := filepath.Glob(dbPath + ".pre-v*.bak"); len(baks) > 0 {
			slog.Warn("the database backups made before this upgrade still hold the Threads token unencrypted: delete them once this version works for you",
				"files", baks)
		}
	}
	return ks, sealed, nil
}

// warnAdoptedStillSet warns about the variables of accounts adopted into
// the app that are still set: they are ignored now.
func warnAdoptedStillSet(ctx context.Context, st *store.Store, env []drivers.EnvAccount, mediaSigned bool) {
	rows, err := st.AccountsWithDeleted(ctx, store.DefaultOwner)
	if err != nil {
		slog.Error("reading accounts failed", "err", err)
		return
	}
	for _, s := range drivers.AdoptedStillSet(rows, env, mediaSigned) {
		msg := "an account adopted into the app is still configured by environment variables, which are ignored for it now: remove them"
		if s.Deleted {
			msg = "an account adopted into the app, then deleted there, is still configured by environment variables, which are ignored: remove them"
		}
		args := []any{"account", s.AccountID, "type", s.Type, "variables", strings.Join(s.Vars, ", ")}
		if s.Keep != "" {
			args = append(args, "note", s.Keep)
		}
		slog.Warn(msg, args...)
	}
}
