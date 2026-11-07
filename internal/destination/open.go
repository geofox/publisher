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
	"syscall"
	"time"
)

// OpenRequest is everything a driver needs to build one account. It is
// not serialized (Host is an interface).
type OpenRequest struct {
	// AccountID is the stored account's id ("acc_…").
	AccountID string
	// Method is the AuthMethod.ID the account was connected with ("" for
	// the driver's only method).
	Method string
	// Config is the account's non-secret configuration. Values may be Go
	// values (from FromEnv) or JSON-decoded ones (float64 or json.Number,
	// []any): read them with ConfigString, ConfigStrings, ConfigInt,
	// ConfigBool and ConfigTime.
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
	PublishTimeout  time.Duration `json:"publish_timeout"`
	POWTimeout      time.Duration `json:"pow_timeout"`
	RelayCacheTTL   time.Duration `json:"relay_cache_ttl"`
	VerifyTimeout   time.Duration `json:"verify_timeout"`
	PLCDirectoryURL string        `json:"plc_directory_url,omitempty"`
}

// StateUpdate is new state an account wants kept: credentials it refreshed
// and configuration that goes with them (a token's expiry). The host writes
// both in one durable step.
//
// It is a patch, merged key by key into what is stored: a key the update
// doesn't name keeps its value, a named key gets the new value, and a
// secret set to "" or a config value set to nil is deleted. So a Threads
// refresh can send only its token and expiry without losing the user id.
// MergeState applies it.
type StateUpdate struct {
	Secrets map[string]string `json:"secrets,omitempty"`
	Config  map[string]any    `json:"config,omitempty"`
}

// MergeState returns the stored secrets and config with u applied (see
// StateUpdate). The inputs are not modified.
func MergeState(secrets map[string]string, config map[string]any, u StateUpdate) (map[string]string, map[string]any) {
	outS := make(map[string]string, len(secrets)+len(u.Secrets))
	for k, v := range secrets {
		outS[k] = v
	}
	for k, v := range u.Secrets {
		if v == "" {
			delete(outS, k)
		} else {
			outS[k] = v
		}
	}
	outC := make(map[string]any, len(config)+len(u.Config))
	for k, v := range config {
		outC[k] = v
	}
	for k, v := range u.Config {
		if v == nil {
			delete(outC, k)
		} else {
			outC[k] = v
		}
	}
	return outS, outC
}

// ErrStateUnsupported is Save's answer for an account that has nowhere to
// keep state (an account from the environment without a master key, other
// than 2.x's one Threads account).
var ErrStateUnsupported = errors.New("this account can't save state without a master key (PUBLISHER_MASTER_KEY)")

// AccountHost is the core's side of an open account.
type AccountHost interface {
	// Save merges new secrets and config into the stored state (see
	// StateUpdate), atomically and durably. ErrStateUnsupported: nowhere
	// to keep it.
	Save(ctx context.Context, u StateUpdate) error
	// Net is how the account reaches the network for one kind of address.
	// Drivers build every HTTP client, transport and WebSocket dial from
	// it, so the host's rules apply to all of them. The driver says where
	// an address came from; the host decides how far that is trusted.
	Net(kind NetPolicy) Net
	// Logger is the account's logger.
	Logger() *slog.Logger
}

// NetPolicy says where an address the account talks to came from. The
// driver knows that; only the host knows who set the account up, so the
// host turns it into rules.
type NetPolicy int

const (
	// NetAccount is for the account's own addresses: those in its config
	// (an instance, a PDS, relays) and its network's fixed API hosts. The
	// host trusts them when the operator set the account up in the
	// environment, and guards them (no private or local destinations
	// unless allowed) when the account was added in the app.
	NetAccount NetPolicy = iota
	// NetContent is for addresses that come from content or from other
	// servers: a relay hint in a pasted link, a link in a post. The host
	// always refuses private and local destinations for them.
	NetContent
)

// Net is a network policy in a form HTTP clients and WebSocket libraries
// take.
type Net struct {
	// DialContext dials a connection; it enforces the policy. A nil
	// DialContext refuses every connection, so a Net nobody filled in
	// can't skip the policy.
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

// errNoNet is the dial error of a Net without a dialer.
var errNoNet = errors.New("no network policy was given for this connection")

// Dial is n.DialContext, or a dialer that refuses when there is none.
func (n Net) Dial() func(ctx context.Context, network, addr string) (net.Conn, error) {
	if n.DialContext != nil {
		return n.DialContext
	}
	return func(context.Context, string, string) (net.Conn, error) { return nil, errNoNet }
}

// Transport builds an HTTP transport that dials through the policy. It
// speaks HTTP/2 where the server offers it, and pings an HTTP/2 connection
// that has been silent for 30 s, closing it if no answer comes in 15 s, so
// a dead connection can't hold every request to that server.
func (n Net) Transport(o TransportOptions) *http.Transport {
	if o.TLSHandshakeTimeout == 0 {
		o.TLSHandshakeTimeout = 10 * time.Second
	}
	return &http.Transport{
		Proxy:                 n.Proxy,
		DialContext:           n.Dial(),
		ForceAttemptHTTP2:     true,
		HTTP2:                 &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second},
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
// keeps the Authorization header when only the scheme changes), and
// replays a request body (a 307 or 308) only to the first request's host
// or one of its subdomains: the hosts Go itself keeps the Authorization
// header for. A body sent anywhere else could carry a password (a
// sign-in) to a host the operator never named.
func SafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	prev := via[len(via)-1]
	if prev.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refused a redirect from https to %s", req.URL.Scheme)
	}
	if req.GetBody != nil && req.ContentLength != 0 && !sameSite(req.URL.Hostname(), via[0].URL.Hostname()) {
		return errors.New("refused to send a request body to another host on a redirect")
	}
	return nil
}

// sameSite reports whether host is parent or one of its subdomains (Go's
// rule for keeping credentials on a redirect). IP addresses match only
// themselves.
func sameSite(host, parent string) bool {
	host, parent = strings.ToLower(host), strings.ToLower(parent)
	if host == parent {
		return true
	}
	if parent == "" || strings.ContainsAny(host, ":%") || net.ParseIP(host) != nil || net.ParseIP(parent) != nil {
		return false
	}
	return strings.HasSuffix(host, "."+parent)
}

// ContentNet reaches addresses that come from content (NetContent): public
// addresses only, checked on the address actually dialed (so a name that
// resolves to a private address later is still refused), with no proxy
// (a proxy would dial for us, past the check) and SafeRedirect. blocked
// says which addresses are refused (netguard.IsBlockedIP).
func ContentNet(blocked func(net.IP) bool) Net {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || blocked(ip) {
				return ErrBlockedAddress
			}
			return nil
		}}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		return c, hideAddress(network, err)
	}
	return Net{DialContext: dial, CheckRedirect: SafeRedirect}
}

// hideAddress drops the address from a refused dial's error (net.Dialer
// names the address it refused): the error can reach the owner, and what
// a name resolves to inside the server's network is not theirs to learn.
func hideAddress(network string, err error) error {
	if errors.Is(err, ErrBlockedAddress) {
		return &net.OpError{Op: "dial", Net: network, Err: ErrBlockedAddress}
	}
	return err
}

// GuardedNet reaches addresses through allow, which judges every
// connection by the host the caller dialed (a name or an address) and the
// address actually connected to, on every redirect too: a name that
// resolves to a refused address later is still refused. No proxy (a proxy
// would dial for us, past the check); GuardedRedirect. allow's error is
// returned as ErrBlockedAddress.
func GuardedNet(allow func(host string, ip net.IP) error) Net {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				h, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(h)
				if ip == nil || allow(host, ip) != nil {
					return ErrBlockedAddress
				}
				return nil
			}}
		c, err := d.DialContext(ctx, network, addr)
		return c, hideAddress(network, err)
	}
	return Net{DialContext: dial, CheckRedirect: GuardedRedirect}
}

// maxGuardedRedirects bounds the redirects GuardedRedirect follows.
const maxGuardedRedirects = 5

// GuardedRedirect is the redirect rule for owner-set and content
// addresses: at most 5 redirects, never from https to http, never a
// redirect of a request other than GET or HEAD (its body may hold a
// password), and a request carrying a credential (Authorization, a
// cookie, a body) never leaves its origin.
func GuardedRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxGuardedRedirects {
		return fmt.Errorf("stopped after %d redirects", maxGuardedRedirects)
	}
	first, prev := via[0], via[len(via)-1]
	if prev.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refused a redirect from https to %s", req.URL.Scheme)
	}
	if first.Method != http.MethodGet && first.Method != http.MethodHead {
		return fmt.Errorf("refused to follow a redirect of a %s request", first.Method)
	}
	credential := first.Header.Get("Authorization") != "" || first.Header.Get("Cookie") != "" ||
		first.GetBody != nil || first.ContentLength > 0
	if credential && !sameOrigin(req.URL, first.URL) {
		return errors.New("refused to send a credential to another server on a redirect")
	}
	return nil
}

// sameOrigin compares scheme, host and port (the default port spelled out
// or not).
func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "wss":
			return "443"
		case "http", "ws":
			return "80"
		}
		return ""
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// ErrBlockedAddress is the dial error for an address a policy refuses. It
// names no address: the error can reach the owner, and what a name
// resolves to inside the server's network is not theirs to learn.
var ErrBlockedAddress = errors.New("refused to connect to a private or local address")

// EnvSource is implemented by drivers that can configure accounts from
// environment variables.
type EnvSource interface {
	// FromEnv returns the accounts the environment configures. An invalid
	// or incomplete combination is an error naming the variables.
	FromEnv(getenv func(string) string) ([]EnvAccount, error)
	// EnvVars lists the variables FromEnv reads, the ones an account needs
	// marked Required, for messages such as "set MASTODON_TOKEN and
	// MASTODON_BASE_URL".
	EnvVars() []EnvVar
}

// EnvVar is one environment variable a driver reads.
type EnvVar struct {
	Name string `json:"name"`
	// Required: an account from the environment needs it.
	Required bool `json:"required,omitempty"`
	// Secret: the variable holds a credential. The server removes it from
	// its environment once read, so no subprocess inherits it.
	Secret bool `json:"secret,omitempty"`
}

// EnvAccount is one account the environment configures.
type EnvAccount struct {
	// Key tells accounts of one type apart ("default" for the unnumbered
	// variables). It is stored as the account's env_key.
	Key string `json:"key"`
	// Label names a newly created account ("" = the manifest's name).
	Label   string            `json:"label,omitempty"`
	Method  string            `json:"method,omitempty"`
	Config  map[string]any    `json:"config,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// EnvKeyDefault is the key of the account configured by a type's
// unnumbered variables.
const EnvKeyDefault = "default"
