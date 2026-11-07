package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/geofox/publisher/internal/config"
	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
)

// Maintenance commands, run in the container next to the server (docker
// exec publisher /publisher …): they read the same environment and
// database. Whoever can run them already controls the instance.

const cliUsage = `usage:
  publisher keygen                            print a new master key (PUBLISHER_MASTER_KEY)
                                              and how to give it to the container
  publisher setup-url                         print a new first-run setup link
  publisher owner reset-login [--revoke-tokens]
                                              remove every passkey, the password and
                                              every session, forget every known browser,
                                              and print a setup link; it lists the active
                                              API tokens, and --revoke-tokens revokes them
`

// runCLI runs the maintenance command args names. handled is false when
// args name none (the server starts).
func runCLI(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) (code int, handled bool) {
	switch {
	case len(args) == 1 && args[0] == "keygen":
		cliKeygen(stdout)
		return 0, true
	case len(args) == 1 && args[0] == "setup-url":
		return cliSetupURL(ctx, getenv, stdout, stderr), true
	case len(args) >= 2 && args[0] == "owner" && args[1] == "reset-login":
		revoke := false
		for _, a := range args[2:] {
			if a != "--revoke-tokens" {
				fmt.Fprint(stderr, cliUsage)
				return 2, true
			}
			revoke = true
		}
		return cliResetLogin(ctx, getenv, revoke, stdout, stderr), true
	case len(args) >= 1 && (args[0] == "keygen" || args[0] == "setup-url" || args[0] == "owner" || args[0] == "help" || args[0] == "-h" || args[0] == "--help"):
		fmt.Fprint(stderr, cliUsage)
		return 2, true
	}
	return 0, false
}

// cliKeygen prints a new master key and how to set it. It needs no
// configuration or database: run it anywhere (docker run --rm <image>
// /publisher keygen).
func cliKeygen(stdout io.Writer) {
	key := secrets.Generate()
	fmt.Fprintf(stdout, `A new master key. Keep it secret, and keep a copy somewhere safe: without
it, the credentials saved in the app can't be read (a backup of the
database needs the key that was set when it was taken).

  %s

Recommended, as a Docker secret file (not shown by docker inspect):

  printf '%%s' '%s' > publisher_master_key && chmod 600 publisher_master_key

  services:
    publisher:
      environment:
        PUBLISHER_MASTER_KEY_FILE: /run/secrets/publisher_master_key
      secrets: [publisher_master_key]
  secrets:
    publisher_master_key:
      file: ./publisher_master_key

Or as a variable: PUBLISHER_MASTER_KEY=%s
`, key, key, key)
}

// cliOpen loads the configuration and opens the database for a command
// that works only in local mode.
func cliOpen(ctx context.Context, getenv func(string) string, stderr io.Writer, cmd string) (config.Config, *store.Store, int) {
	cfg, err := config.LoadFrom(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "config error:", err)
		return cfg, nil, 2
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintln(stderr, "open the database:", err)
		return cfg, nil, 1
	}
	born, err := st.CreatedSchema(ctx)
	if err != nil {
		_ = st.Close()
		fmt.Fprintln(stderr, "read the database:", err)
		return cfg, nil, 1
	}
	if mode := cfg.ResolveAuthMode(born); mode != config.AuthLocal {
		_ = st.Close()
		fmt.Fprintf(stderr, "%s: sign-in here is AUTH_MODE=%s, not local; this command is for Publisher's own passkeys and password\n", cmd, mode)
		return cfg, nil, 2
	}
	if err := cfg.CheckLocal(); err != nil { // the server refuses to start with it, so a link would be useless
		_ = st.Close()
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
		return cfg, nil, 2
	}
	return cfg, st, 0
}

func cliSetupURL(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, st, code := cliOpen(ctx, getenv, stderr, "setup-url")
	if st == nil {
		return code
	}
	defer st.Close()
	done, err := st.HasLocalCredential(ctx, localauth.RPIDOf(cfg.PublicURL))
	if err != nil {
		fmt.Fprintln(stderr, "read the database:", err)
		return 1
	}
	if done {
		fmt.Fprintln(stderr, "setup-url: sign-in is already set up. Lost every passkey and the password? Run: publisher owner reset-login")
		return 1
	}
	return printSetupLink(ctx, st, cfg, stdout, stderr)
}

func printSetupLink(ctx context.Context, st *store.Store, cfg config.Config, stdout, stderr io.Writer) int {
	raw, exp, err := st.MintSetupToken(ctx, localauth.SetupTokenTTL)
	if err != nil {
		fmt.Fprintln(stderr, "create a setup link:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Open this link, once, before %s:\n\n  %s\n\n", exp.Local().Format(time.DateTime), localauth.SetupURL(cfg.PublicURL, raw))
	if cfg.PublicURL == "" {
		fmt.Fprintln(stdout, "PUBLIC_URL is unset: put the https address you use for Publisher in front of /setup, and set PUBLIC_URL (sign-in needs it).")
	}
	fmt.Fprintln(stdout, "Any older setup link no longer works.")
	return 0
}

func cliResetLogin(ctx context.Context, getenv func(string) string, revoke bool, stdout, stderr io.Writer) int {
	cfg, st, code := cliOpen(ctx, getenv, stderr, "owner reset-login")
	if st == nil {
		return code
	}
	defer st.Close()
	if err := st.ResetLocalLogin(ctx); err != nil {
		fmt.Fprintln(stderr, "reset-login:", err)
		return 1
	}
	if err := st.AddSignInEvent(ctx, store.DefaultOwner, store.EventSignInRemoved, store.EventData{Method: "reset-login"}); err != nil {
		fmt.Fprintln(stderr, "reset-login: record the event:", err)
	}
	fmt.Fprintln(stdout, "Removed every passkey, the password and every signed-in session.")

	// Whoever got in may have made a token, which outlives the sessions:
	// list every active one.
	tokens, err := st.ListAPITokens()
	if err != nil {
		fmt.Fprintln(stderr, "list the API tokens:", err)
		return 1
	}
	now := time.Now()
	var live []store.APIToken
	for _, t := range tokens {
		if !t.Revoked && (t.ExpiresAt.IsZero() || t.ExpiresAt.After(now)) {
			live = append(live, t)
		}
	}
	switch {
	case len(live) == 0:
		fmt.Fprintln(stdout, "No API token is active.")
	case revoke:
		var errs []error
		for _, t := range live {
			if err := st.RevokeAPIToken(t.ID); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.Name, err))
				continue
			}
			fmt.Fprintf(stdout, "Revoked token %q (%v).\n", t.Name, t.Scopes)
		}
		if err := errors.Join(errs...); err != nil {
			fmt.Fprintln(stderr, "revoke:", err)
			return 1
		}
	default:
		fmt.Fprintln(stdout, "These API tokens are active; if one may be in the wrong hands, revoke it in the app (or run again with --revoke-tokens to revoke them all):")
		for _, t := range live {
			fmt.Fprintf(stdout, "  %q %v, created %s\n", t.Name, t.Scopes, t.CreatedAt.Local().Format(time.DateOnly))
		}
	}
	fmt.Fprintln(stdout)
	return printSetupLink(ctx, st, cfg, stdout, stderr)
}
