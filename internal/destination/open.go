package destination

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpenRequest is everything a driver needs to build one account.
type OpenRequest struct {
	// AccountID is the stored account's id ("acc_…").
	AccountID string
	// Method is the AuthMethod.ID the account was connected with ("" for
	// the driver's only method).
	Method string
	// Config is the account's non-secret configuration. Values may be Go
	// values (from FromEnv) or JSON-decoded ones (float64, []any): read them
	// with ConfigString, ConfigStrings, ConfigInt and ConfigBool.
	Config map[string]any
	// Secrets are the account's credentials, by name.
	Secrets map[string]string
	// Tuning holds the timeouts and endpoints the operator set.
	Tuning Tuning
	// Host lets the account persist state and reach the network.
	Host AccountHost
}

// Tuning is operator configuration that applies to every account.
type Tuning struct {
	PublishTimeout  time.Duration
	POWTimeout      time.Duration
	RelayCacheTTL   time.Duration
	VerifyTimeout   time.Duration
	PLCDirectoryURL string
}

// StateUpdate is new state an account wants kept: credentials it refreshed
// and configuration that goes with them (a token's expiry). The host writes
// both in one durable step.
type StateUpdate struct {
	Secrets map[string]string
	Config  map[string]any
}

// AccountHost is the core's side of an open account.
type AccountHost interface {
	// Save persists new secrets and config together, atomically.
	Save(ctx context.Context, u StateUpdate) error
	// Net is how the account reaches the network under a policy. Drivers
	// build every HTTP client, transport and WebSocket dial from it, so the
	// host's rules apply to all of them. The host may tighten the policy a
	// driver asks for, never loosen it.
	Net(policy NetPolicy) Net
	// Logger is the account's logger.
	Logger() *slog.Logger
}

// NetPolicy says how far an address the account talks to can be trusted.
type NetPolicy int

const (
	// NetTrusted is for addresses the operator configured (environment
	// variables, a network's fixed API host).
	NetTrusted NetPolicy = iota
	// NetOwner is for addresses typed in the app (an instance URL, a relay):
	// the host refuses private and local destinations unless allowed.
	NetOwner
)

// Net is a network policy in a form HTTP clients and WebSocket libraries
// take.
type Net struct {
	// DialContext dials a connection; it enforces the policy.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Proxy picks the proxy for a request (nil: no proxy).
	Proxy func(*http.Request) (*url.URL, error)
	// CheckRedirect decides whether a redirect is followed (nil: Go's
	// default of at most 10).
	CheckRedirect func(req *http.Request, via []*http.Request) error
}

// TransportOptions are the timeouts of an HTTP transport built from a Net.
type TransportOptions struct {
	// TLSHandshakeTimeout and ResponseHeaderTimeout default to 10 s and
	// 0 (none).
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
}

// Transport builds an HTTP transport that dials through the policy.
func (n Net) Transport(o TransportOptions) *http.Transport {
	if o.TLSHandshakeTimeout == 0 {
		o.TLSHandshakeTimeout = 10 * time.Second
	}
	return &http.Transport{
		Proxy:                 n.Proxy,
		DialContext:           n.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   o.TLSHandshakeTimeout,
		ResponseHeaderTimeout: o.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
}

// Client builds an HTTP client with an overall timeout (0: none).
func (n Net) Client(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport:     n.Transport(TransportOptions{}),
		Timeout:       timeout,
		CheckRedirect: n.CheckRedirect,
	}
}

// TrustedNet is the network as Publisher reached it before policies
// existed: a 10-second dial timeout and the proxy from the environment,
// with SafeRedirect as the redirect rule.
func TrustedNet() Net {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return Net{DialContext: d.DialContext, Proxy: http.ProxyFromEnvironment, CheckRedirect: SafeRedirect}
}

// SafeRedirect follows at most 10 redirects, never from https to http (Go
// keeps the Authorization header when only the scheme changes), and never
// replays a request body to another host (a 307 or 308 after a sign-in
// would send the password there). Go itself drops credentials on a
// redirect to another host.
func SafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	prev := via[len(via)-1]
	if prev.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refused a redirect from https to %s", req.URL.Scheme)
	}
	if req.GetBody != nil && req.ContentLength != 0 && !strings.EqualFold(req.URL.Host, prev.URL.Host) {
		return errors.New("refused to send a request body to another host on a redirect")
	}
	return nil
}

// EnvSource is implemented by drivers that can configure accounts from
// environment variables.
type EnvSource interface {
	// FromEnv returns the accounts the environment configures. An invalid
	// or incomplete combination is an error naming the variables.
	FromEnv(getenv func(string) string) ([]EnvAccount, error)
	// EnvVars names the variables FromEnv reads, for messages such as
	// "set MASTODON_TOKEN and MASTODON_BASE_URL".
	EnvVars() []string
}

// EnvAccount is one account the environment configures.
type EnvAccount struct {
	// Key tells accounts of one type apart ("default" for the unnumbered
	// variables). It is stored as the account's env_key.
	Key string
	// Label names a newly created account ("" = the manifest's name).
	Label   string
	Method  string
	Config  map[string]any
	Secrets map[string]string
}

// EnvKeyDefault is the key of the account configured by a type's
// unnumbered variables.
const EnvKeyDefault = "default"
