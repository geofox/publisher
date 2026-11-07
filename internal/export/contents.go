package export

import (
	"encoding/json"
	"errors"
)

// ContentsVersion is the version of what a file holds.
const ContentsVersion = 1

// Contents is what a file holds (spec §9): an owner's accounts, their
// account sets, and the settings saved in the app (secrets included). No
// posts, drafts, media or API tokens.
type Contents struct {
	Version  int            `json:"version"`
	Accounts []Account      `json:"accounts"`
	Sets     []Set          `json:"account_sets"`
	Owner    map[string]any `json:"owner_settings"`
	Instance map[string]any `json:"instance_settings"`
}

// Account sources in a file.
const (
	// SourceApp: added in the app; the file holds its credentials.
	SourceApp = "app"
	// SourceEnv: set by the environment where it was exported; the file
	// holds only what the owner chose for it (label, flags, defaults),
	// for the same account here.
	SourceEnv = "env"
)

// Account is one account in a file.
type Account struct {
	// ID is its id where it was exported: account sets name it so.
	ID     string `json:"id"`
	Source string `json:"source"`
	// EnvKey is an environment account's variables ("default", or a
	// number), to find it here when it wasn't identified yet.
	EnvKey   string `json:"env_key,omitempty"`
	Type     string `json:"type"`
	Label    string `json:"label"`
	Handle   string `json:"handle,omitempty"`
	RemoteID string `json:"remote_id,omitempty"`
	// Method, Config and Secrets: an app account's way of connecting,
	// its configuration (the state its driver saved too) and every
	// credential, in the clear (the file is sealed).
	Method  string            `json:"auth_method,omitempty"`
	Config  map[string]any    `json:"config,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`

	Enabled         bool            `json:"enabled"`
	DefaultSelected bool            `json:"default_selected"`
	PublicFeed      bool            `json:"public_feed"`
	Defaults        json.RawMessage `json:"defaults,omitempty"`
	// SyncRelays is a Nostr account's relay-sync list ([] when the
	// owner emptied it; null for another network).
	SyncRelays []string `json:"sync_relays"`
}

// Set is an account set, by the accounts' ids in the file.
type Set struct {
	Name      string   `json:"name"`
	Accounts  []string `json:"accounts"`
	IsDefault bool     `json:"is_default"`
}

// Bounds of a file's contents, checked before anything is imported (and
// before an export is made, so every file can be imported).
const (
	MaxAccounts = 100
	MaxSets     = 50
)

// Decode reads a file's contents, checking their shape.
func Decode(plain []byte) (Contents, error) {
	var c Contents
	if err := json.Unmarshal(plain, &c); err != nil {
		// Not the decoder's words: they may quote the contents.
		return Contents{}, errors.New("the file's contents can't be read")
	}
	switch {
	case c.Version > ContentsVersion:
		return Contents{}, ErrVersion
	case c.Version != ContentsVersion:
		return Contents{}, errors.New("the file's contents can't be read")
	}
	if len(c.Accounts) > MaxAccounts || len(c.Sets) > MaxSets {
		return Contents{}, errors.New("the file holds more accounts or sets than Publisher keeps")
	}
	ids := map[string]bool{}
	for _, a := range c.Accounts {
		if a.ID == "" || ids[a.ID] {
			return Contents{}, errors.New("the file's accounts have missing or repeated ids")
		}
		ids[a.ID] = true
		if a.Source != SourceApp && a.Source != SourceEnv {
			return Contents{}, errors.New("an account in the file comes from an unknown source")
		}
	}
	return c, nil
}
