package accounts

import (
	"context"
	"errors"
	"fmt"

	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// KeyStatus says whether the master key opens the stored credentials.
type KeyStatus string

const (
	// KeyNone: no master key and nothing stored; accounts come from the
	// environment only.
	KeyNone KeyStatus = "none"
	// KeyOK: the key opens everything stored, and new credentials can be
	// saved.
	KeyOK KeyStatus = "ok"
	// KeyMissing: credentials are stored but no key is set.
	KeyMissing KeyStatus = "missing"
	// KeyMismatch: credentials are stored under a key this server doesn't
	// have, or a rotation failed.
	KeyMismatch KeyStatus = "mismatch"
)

// KeyState is the master key's status, with what to do about it in the
// operator's terms (never a key or a value).
type KeyState struct {
	Status KeyStatus `json:"status"`
	Detail string    `json:"detail,omitempty"`
	// Rotated counts the values a rotation moved to the new key at startup.
	Rotated int `json:"-"`
}

// Errors for a credential write the key status refuses.
var (
	ErrMasterKeyRequired = errors.New("set a master key (PUBLISHER_MASTER_KEY) to save credentials; `publisher keygen` makes one")
	ErrMasterKeyMismatch = errors.New("the stored credentials were saved with another master key; fix PUBLISHER_MASTER_KEY before saving new ones, so the database never mixes keys")
)

// WriteError is the error a credential write gets under this status (nil
// when it may go ahead).
func (k KeyState) WriteError() error {
	switch k.Status {
	case KeyOK:
		return nil
	case KeyMismatch:
		return ErrMasterKeyMismatch
	default:
		return ErrMasterKeyRequired
	}
}

// KeyStore is what PrepareKeys reads and rewrites.
type KeyStore interface {
	SecretKeyIDs(ctx context.Context) (map[string]int, error)
	ResealSecrets(ctx context.Context, fn func(store.SealedSecret) (sealed, keyID string, changed bool, err error)) (int, error)
}

// Binding is the binding a stored secret was sealed for.
func Binding(s store.SealedSecret) secrets.Binding {
	return secrets.Binding{Owner: s.Owner, Scope: s.Scope, Ref: s.Ref, Name: s.Name, Audience: s.Audience}
}

// PrepareKeys runs at startup, before the registry loads. With a previous
// key set, every value sealed under it is sealed again under the current
// key, in one transaction: if any value fails, nothing changes and the
// status is a mismatch. Then the status is read from the key ids of what
// is stored. An error means the store couldn't be read.
func PrepareKeys(ctx context.Context, st KeyStore, kr secrets.Keyring) (KeyState, error) {
	var state KeyState
	if kr.Previous != nil && kr.Current != nil {
		n, err := st.ResealSecrets(ctx, func(s store.SealedSecret) (string, string, bool, error) {
			out, changed, err := kr.Reseal(Binding(s), s.Sealed)
			if err != nil || !changed {
				return "", "", false, err
			}
			kid, err := secrets.KeyID(out)
			return out, kid, true, err
		})
		if err != nil {
			return KeyState{Status: KeyMismatch, Detail: fmt.Sprintf("rotating to the new master key failed, so nothing was changed (%v). "+
				"Check that PUBLISHER_MASTER_KEY_PREVIOUS is the key the credentials were saved with. Until then the accounts it can't open are locked "+
				"and no credential can be saved.", err)}, nil
		}
		state.Rotated = n
	}
	ids, err := st.SecretKeyIDs(ctx)
	if err != nil {
		return KeyState{}, err
	}
	total, foreign := 0, 0
	for kid, n := range ids {
		total += n
		if kr.Current == nil || kid != kr.Current.ID() {
			foreign += n
		}
	}
	switch {
	case total == 0 && kr.Current == nil:
		state.Status = KeyNone
		state.Detail = "no master key is set: accounts come from the environment only. Set PUBLISHER_MASTER_KEY (`publisher keygen` makes one) to add accounts in the app."
	case kr.Current == nil:
		state.Status = KeyMissing
		state.Detail = fmt.Sprintf("%d saved credentials need the master key they were saved with: set PUBLISHER_MASTER_KEY (or PUBLISHER_MASTER_KEY_FILE). "+
			"Until then the accounts added in the app are locked and their posts wait.", total)
	case foreign > 0:
		state.Status = KeyMismatch
		state.Detail = fmt.Sprintf("%d saved credentials were saved with another master key: set PUBLISHER_MASTER_KEY to that key, or set it as "+
			"PUBLISHER_MASTER_KEY_PREVIOUS beside the new one to rotate. Until then their accounts are locked and no credential can be saved.", foreign)
	default:
		state.Status = KeyOK
	}
	return state, nil
}
