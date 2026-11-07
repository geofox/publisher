package destination

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
)

// Connector is implemented by a driver with a FlowRedirect method (spec
// §6.1): the owner signs in at the network, which sends them back with a
// code. The driver builds the sign-in address and turns the answer into
// the account's config, secrets and identity. The host owns everything
// else: the state, PKCE, the redirect URI, sealing what the driver keeps
// between the two steps, and storing the account.
type Connector interface {
	BeginConnect(ctx context.Context, req ConnectRequest) (ConnectStart, error)
	CompleteConnect(ctx context.Context, req ConnectComplete) (ConnectResult, error)
}

// ConnectRequest is what both steps get.
type ConnectRequest struct {
	// Method is the AuthMethod's ID.
	Method string
	// Config holds the validated config fields and the method's
	// non-secret fields (a Mastodon instance's address).
	Config map[string]any
	// InstanceConfig and InstanceSecrets are the type's InstanceConfig
	// values (a Threads app's id and secret).
	InstanceConfig  map[string]any
	InstanceSecrets map[string]string
	// RedirectURI is where the network sends the owner back, used
	// verbatim in both steps.
	RedirectURI string
	// State goes in the sign-in address as is.
	State string
	// CodeChallenge is the S256 challenge of the host's PKCE verifier.
	CodeChallenge string
	// Net is how every request of the flow reaches the network
	// (discovery, registration, the code exchange, the identity): as an
	// account added in the app does.
	Net Net
	// Clients keeps the OAuth clients registered with servers (nil for a
	// type that registers none).
	Clients OAuthClients
	Logger  *slog.Logger
}

// ConnectStart is the first step's answer.
type ConnectStart struct {
	// AuthorizeURL is where the owner signs in.
	AuthorizeURL string
	// Data is what the driver needs at the callback (an issuer, a token
	// endpoint, a client id); the host seals it until then.
	Data json.RawMessage
}

// ConnectComplete is the callback's step.
type ConnectComplete struct {
	ConnectRequest
	Data json.RawMessage
	// Callback is the callback's query (code, state, iss, error).
	Callback url.Values
	// CodeVerifier is the PKCE verifier CodeChallenge was made from.
	CodeVerifier string
}

// ConnectResult is an account the network said the owner may connect.
type ConnectResult struct {
	Config   map[string]any
	Secrets  map[string]string
	Identity Identity
	// Candidates, when not empty, are the accounts the owner picks from
	// (a network that offers several); the result's own fields are then
	// unused.
	Candidates []ConnectResult
}

// OAuthClient is a client registered with a server for the flow's
// redirect URI.
type OAuthClient struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       string
}

// OAuthClients keeps the clients a driver registers with servers, per
// issuer, for the flow's type and redirect URI. The host seals the
// secrets.
type OAuthClients interface {
	Client(ctx context.Context, issuer string) (OAuthClient, bool, error)
	SaveClient(ctx context.Context, c OAuthClient) error
	ForgetClient(ctx context.Context, issuer string) error
}

// Connect error codes (spec §6.1): fixed words the owner sees; a network's
// own text is never shown with them.
const (
	ConnectDenied           = "denied"
	ConnectExpired          = "expired"
	ConnectStateInvalid     = "state_invalid"
	ConnectSessionMismatch  = "session_mismatch"
	ConnectIdentityChanged  = "identity_changed"
	ConnectDuplicateAccount = "duplicate_account"
	ConnectExchangeFailed   = "exchange_failed"
	ConnectIssuerMismatch   = "issuer_mismatch"
	ConnectMasterKey        = "master_key_required"
)

// ConnectError is a flow that failed with one of the codes; Err, for the
// log, may hold the network's answer (with no secret in it).
type ConnectError struct {
	Code string
	Err  error
}

func (e *ConnectError) Error() string {
	if e.Err == nil {
		return "connect: " + e.Code
	}
	return "connect: " + e.Code + ": " + e.Err.Error()
}

func (e *ConnectError) Unwrap() error { return e.Err }

// ConnectCode is err's code (exchange_failed when it has none).
func ConnectCode(err error) string {
	var ce *ConnectError
	if errors.As(err, &ce) && ce.Code != "" {
		return ce.Code
	}
	return ConnectExchangeFailed
}
