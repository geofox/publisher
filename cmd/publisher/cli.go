package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/geofox/publisher/internal/config"
	"github.com/geofox/publisher/internal/localauth"
	"github.com/geofox/publisher/internal/store"
)

// Maintenance commands, run in the container next to the server (docker
// exec publisher /publisher …): they read the same environment and
// database. Whoever can run them already controls the instance.

const cliUsage = `usage:
  publisher setup-url                         print a new first-run setup link
  publisher owner reset-login [--revoke-tokens]
                                              remove every passkey, the password and
                                              every session, and print a setup link;
                                              --revoke-tokens also revokes the tokens
                                              that can make tokens or change accounts
`

// privilegedTokenScopes are the scopes reset-login lists (and revokes):
// a token holding one can make tokens or change accounts.
var privilegedTokenScopes = []string{"admin", "accounts:write"}

// runCLI runs the maintenance command args names. handled is false when
// args name none (the server starts).
func runCLI(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) (code int, handled bool) {
	switch {
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
	case len(args) >= 1 && (args[0] == "setup-url" || args[0] == "owner" || args[0] == "help" || args[0] == "-h" || args[0] == "--help"):
		fmt.Fprint(stderr, cliUsage)
		return 2, true
	}
	return 0, false
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
	return cfg, st, 0
}

func cliSetupURL(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, st, code := cliOpen(ctx, getenv, stderr, "setup-url")
	if st == nil {
		return code
	}
	defer st.Close()
	done, err := st.HasLocalCredential(ctx)
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

	tokens, err := st.ListAPITokens()
	if err != nil {
		fmt.Fprintln(stderr, "list the API tokens:", err)
		return 1
	}
	now := time.Now()
	var strong []store.APIToken
	for _, t := range tokens {
		live := !t.Revoked && (t.ExpiresAt.IsZero() || t.ExpiresAt.After(now))
		if live && slices.ContainsFunc(t.Scopes, func(s string) bool { return slices.Contains(privilegedTokenScopes, s) }) {
			strong = append(strong, t)
		}
	}
	switch {
	case len(strong) == 0:
		fmt.Fprintln(stdout, "No active API token can make tokens or change accounts.")
	case revoke:
		var errs []error
		for _, t := range strong {
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
		fmt.Fprintln(stdout, "These active API tokens can make tokens or change accounts; if one may be in the wrong hands, run again with --revoke-tokens (or revoke it in the app):")
		for _, t := range strong {
			fmt.Fprintf(stdout, "  %q %v, created %s\n", t.Name, t.Scopes, t.CreatedAt.Local().Format(time.DateOnly))
		}
	}
	fmt.Fprintln(stdout)
	return printSetupLink(ctx, st, cfg, stdout, stderr)
}
