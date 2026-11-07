package localauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// First-run setup: in local mode with no way to sign in yet, each start
// mints a setup token (1 hour, single use) and says where to open it. The
// setup page exchanges the token once for a setup state (10 minutes, in
// memory, referenced by a cookie scoped to /setup), which binds the first
// passkey's or password's creation.
const (
	SetupTokenTTL  = time.Hour
	SetupStateTTL  = 10 * time.Minute
	maxSetupStates = 16
)

// ErrSetupToken: the setup link is unknown, used or expired.
var ErrSetupToken = errors.New("this setup link expired or was already used: run `publisher setup-url` in the container for a new one")

// SetupURL is where the owner opens a setup token: PUBLIC_URL's /setup, or
// the bare path when PUBLIC_URL is unset.
func SetupURL(publicURL, token string) string { return publicURL + "/setup#" + token }

// AnnounceSetup mints a setup token when setup is pending and tells the
// operator where to open it: in the log, or written to tokenFile (0600)
// with only the path logged. It does nothing once a credential exists.
func AnnounceSetup(ctx context.Context, st *store.Store, publicURL, tokenFile string) error {
	done, err := st.HasLocalCredential(ctx, RPIDOf(publicURL))
	if err != nil || done {
		if tokenFile != "" && done {
			_ = os.Remove(tokenFile)
		}
		return err
	}
	raw, exp, err := st.MintSetupToken(ctx, SetupTokenTTL)
	if err != nil {
		return err
	}
	link := SetupURL(publicURL, raw)
	if tokenFile != "" {
		if err := writeSecretFile(tokenFile, link+"\n"); err != nil {
			return fmt.Errorf("SETUP_TOKEN_FILE: %w", err)
		}
		slog.Warn("setup: sign-in isn't set up yet; the setup link is in SETUP_TOKEN_FILE", "path", tokenFile, "expires", exp.Format(time.RFC3339))
		return nil
	}
	// The token is a one-time bootstrap secret, shown where the operator
	// can see it, valid for an hour and useless once setup is done.
	slog.Warn("setup: open "+link+" on the address you use for Publisher", "expires", exp.Format(time.RFC3339))
	if publicURL == "" {
		slog.Warn("setup: PUBLIC_URL is unset; sign-in needs it (the https address you use for Publisher)")
	}
	return nil
}

// writeSecretFile writes s to path, readable only by its owner.
func writeSecretFile(path, s string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// SetupStates are the setup pages that exchanged a token. Each remembers
// its token, so a newer link (which deletes the older tokens) ends it.
type SetupStates struct {
	mu  sync.Mutex
	m   map[string]setupState
	now func() time.Time
}

type setupState struct {
	token   string
	expires time.Time
}

func (s *SetupStates) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Start returns the id of a new setup state for the setup token it was
// exchanged for.
func (s *SetupStates) Start(token string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]setupState{}
	}
	now := s.clock()
	for id, st := range s.m {
		if !now.Before(st.expires) {
			delete(s.m, id)
		}
	}
	for len(s.m) >= maxSetupStates { // drop the oldest
		var oldest string
		for id, st := range s.m {
			if oldest == "" || st.expires.Before(s.m[oldest].expires) {
				oldest = id
			}
		}
		delete(s.m, oldest)
	}
	id := randomID()
	s.m[id] = setupState{token: token, expires: now.Add(SetupStateTTL)}
	return id
}

// Token is the setup token of live setup state id, if it is one; the
// caller checks the token is still the current one (Store.SetupTokenCurrent).
func (s *SetupStates) Token(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[id]
	if !ok || id == "" || !s.clock().Before(st.expires) {
		return "", false
	}
	return st.token, true
}

// End forgets every setup state: setup is done.
func (s *SetupStates) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = nil
}
