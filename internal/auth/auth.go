// Package auth implements the OpenID Connect Relying Party mechanics: building
// the authorize URL, exchanging the code, and verifying the ID token. Session
// and cookie management live in internal/api — this package owns only the OIDC
// crypto, delegated to coreos/go-oidc + x/oauth2.
package auth

import (
	"context"
	"fmt"
	"net/url"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Claims is the subset of the verified ID token we use.
type Claims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
}

// Config configures the Relying Party.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string // default: openid profile email
}

// Authenticator performs the OIDC RP flow.
type Authenticator struct {
	verifier      *oidc.IDTokenVerifier
	oauth2        oauth2.Config
	endSessionURL string // from discovery; "" if the provider omits it
}

// New discovers the provider (network call to the issuer's
// .well-known/openid-configuration) and builds the verifier + oauth2 config.
// Returning an error here is intentionally fatal at startup.
func New(ctx context.Context, c Config) (*Authenticator, error) {
	provider, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	scopes := c.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	var extra struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = provider.Claims(&extra) // best-effort; not all providers expose it
	return &Authenticator{
		verifier: provider.Verifier(&oidc.Config{ClientID: c.ClientID}),
		oauth2: oauth2.Config{
			ClientID:     c.ClientID,
			ClientSecret: c.ClientSecret,
			RedirectURL:  c.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       scopes,
		},
		endSessionURL: extra.EndSession,
	}, nil
}

// AuthCodeURL builds the authorize redirect with nonce + PKCE S256 challenge.
func (a *Authenticator) AuthCodeURL(state, nonce, pkceVerifier string) string {
	return a.oauth2.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(pkceVerifier))
}

// Exchange swaps the code for tokens, verifies the ID token signature/claims,
// checks the nonce, and returns the verified claims.
func (a *Authenticator) Exchange(ctx context.Context, code, nonce, pkceVerifier string) (Claims, error) {
	tok, err := a.oauth2.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Claims{}, fmt.Errorf("token exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		return Claims{}, fmt.Errorf("no id_token in response")
	}
	idToken, err := a.verifier.Verify(ctx, rawID)
	if err != nil {
		return Claims{}, fmt.Errorf("verify id_token: %w", err)
	}
	if idToken.Nonce != nonce {
		return Claims{}, fmt.Errorf("nonce mismatch")
	}
	var claims Claims
	if err := idToken.Claims(&claims); err != nil {
		return Claims{}, fmt.Errorf("decode claims: %w", err)
	}
	return claims, nil
}

// EndSessionURL returns the provider logout URL with a post-logout redirect, or
// "" if the provider has no end_session_endpoint.
func (a *Authenticator) EndSessionURL(postLogoutRedirect string) string {
	if a.endSessionURL == "" {
		return ""
	}
	u, err := url.Parse(a.endSessionURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("post_logout_redirect_uri", postLogoutRedirect)
	q.Set("client_id", a.oauth2.ClientID)
	u.RawQuery = q.Encode()
	return u.String()
}
