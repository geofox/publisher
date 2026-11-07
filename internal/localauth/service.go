package localauth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/geofox/publisher/internal/store"
)

// Service is local sign-in: passkeys for PUBLIC_URL's host, the password,
// and the throttle in front of it.
type Service struct {
	Store      *store.Store
	Hasher     *Hasher
	Throttle   *Throttle
	Ceremonies *Ceremonies
	Setup      *SetupStates
	// FailedEvents bounds the failed sign-ins recorded as events per
	// address (all are still logged), so one address can't push the
	// others out of the owner's view.
	FailedEvents *Limiter

	rp     *webauthn.WebAuthn // nil without PUBLIC_URL
	origin string
	rpID   string
}

// Errors of sign-in. Callers say no more than "that didn't work" for
// ErrSignIn: which part failed isn't told.
var (
	ErrSignIn      = errors.New("sign-in failed")
	ErrNoPublicURL = errors.New("passkeys need PUBLIC_URL: set it to the https address you use for Publisher")
	ErrNoPassword  = errors.New("no password is set")
	ErrNoPasskey   = errors.New("you have no passkey here; use your password")
	// ErrTooManyPasskeys: the owner has MaxPasskeys already.
	ErrTooManyPasskeys = fmt.Errorf("you have %d passkeys, the most there can be: remove one first", MaxPasskeys)
)

// MaxPasskeys bounds the owner's passkeys.
const MaxPasskeys = 20

// failedEventsPerAddr: at most this many failed sign-ins from one address
// are recorded in ten minutes.
const failedEventsPerAddr = 10

// RPIDOf is the passkeys' relying party id for PUBLIC_URL: its host ("" when
// unset or unusable).
func RPIDOf(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil || publicURL == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// ErrThrottled: wait before trying a password again. Everyone: the delay
// is owner-wide (wrong passwords from many places), not this address's.
type ErrThrottled struct {
	Wait     time.Duration
	Everyone bool
}

func (e *ErrThrottled) Error() string { return "too many wrong passwords; try again later" }

// New returns the service. publicURL ("" when unset) is the relying party:
// its host is the passkeys' relying party id and its origin the only one
// accepted. Without it passkeys are refused (never derived from Host).
func New(st *store.Store, publicURL string) (*Service, error) {
	s := &Service{Store: st, Hasher: NewHasher(), Throttle: &Throttle{Store: st}, Ceremonies: NewCeremonies(), Setup: &SetupStates{},
		FailedEvents: &Limiter{N: failedEventsPerAddr, Window: 10 * time.Minute}}
	if publicURL == "" {
		return s, nil
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("PUBLIC_URL %q: not an origin", publicURL)
	}
	s.origin, s.rpID = publicURL, strings.ToLower(u.Hostname())
	s.rp, err = webauthn.New(&webauthn.Config{
		RPID:          s.rpID,
		RPDisplayName: "Publisher",
		RPOrigins:     []string{s.origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
		AttestationPreference: protocol.PreferNoAttestation,
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: CeremonyTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: CeremonyTTL},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("passkeys for %s: %w", publicURL, err)
	}
	return s, nil
}

// Passkeys reports whether passkeys can be used (PUBLIC_URL is set).
func (s *Service) Passkeys() bool { return s.rp != nil }

// Origin is PUBLIC_URL ("" when unset).
func (s *Service) Origin() string { return s.origin }

// RPID is the passkeys' relying party id ("" without PUBLIC_URL).
func (s *Service) RPID() string { return s.rpID }

// owner is the local owner as go-webauthn sees it.
type owner struct {
	u     store.User
	creds []webauthn.Credential
}

func (o owner) WebAuthnID() []byte                         { return []byte(o.u.ID) }
func (o owner) WebAuthnName() string                       { return "owner" }
func (o owner) WebAuthnDisplayName() string                { return "Publisher owner" }
func (o owner) WebAuthnCredentials() []webauthn.Credential { return o.creds }

func toWebAuthn(c store.WebAuthnCredential) webauthn.Credential {
	var tr []protocol.AuthenticatorTransport
	for _, t := range c.Transports {
		tr = append(tr, protocol.AuthenticatorTransport(t))
	}
	return webauthn.Credential{
		ID: c.CredentialID, PublicKey: c.PublicKey, AttestationType: c.AttestationType, Transport: tr,
		Flags:         webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(c.Flags)),
		Authenticator: webauthn.Authenticator{AAGUID: c.AAGUID, SignCount: c.SignCount},
	}
}

// ownerWith loads the local owner with its passkeys for this relying
// party (one made for another host can't be used here).
func (s *Service) ownerWith(ctx context.Context, u store.User) (owner, []store.WebAuthnCredential, error) {
	all, err := s.Store.WebAuthnCredentials(ctx, u.ID)
	if err != nil {
		return owner{}, nil, err
	}
	o := owner{u: u}
	var mine []store.WebAuthnCredential
	for _, c := range all {
		if c.RPID == s.rpID {
			o.creds = append(o.creds, toWebAuthn(c))
			mine = append(mine, c)
		}
	}
	return o, mine, nil
}

// Options are what the browser passes to navigator.credentials.
type Options any

// BeginLogin starts a discoverable passkey sign-in from addr.
func (s *Service) BeginLogin(addr netip.Addr) (Options, string, error) {
	if s.rp == nil {
		return nil, "", ErrNoPublicURL
	}
	opts, data, err := s.rp.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	id, err := s.Ceremonies.start(AddrKey(addr), ceremony{Purpose: PurposeLogin, Data: *data})
	return opts, id, err
}

// openCeremony opens a ceremony for a finish from addr: refused while the
// address is over its limit; a bad one counts against it.
func (s *Service) openCeremony(addr netip.Addr, sealed string, purpose Purpose, binding, op string) (ceremony, error) {
	if err := s.Ceremonies.limited(AddrKey(addr)); err != nil {
		return ceremony{}, err
	}
	cer, err := s.Ceremonies.open(sealed, purpose, binding, op)
	if err != nil {
		s.Ceremonies.failed(AddrKey(addr))
	}
	return cer, err
}

// rejected counts a response that didn't check out against addr.
func (s *Service) rejected(addr netip.Addr) error {
	s.Ceremonies.failed(AddrKey(addr))
	return ErrSignIn
}

// FinishLogin checks a sign-in assertion (the browser's JSON) from addr
// against the sealed ceremony and returns the local owner.
func (s *Service) FinishLogin(ctx context.Context, addr netip.Addr, sealed string, body []byte) (store.User, error) {
	if s.rp == nil {
		return store.User{}, ErrNoPublicURL
	}
	cer, err := s.openCeremony(addr, sealed, PurposeLogin, "", "")
	if err != nil {
		return store.User{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		return store.User{}, s.rejected(addr)
	}
	var used store.WebAuthnCredential
	var who store.User
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		c, err := s.Store.WebAuthnCredentialByCredID(ctx, rawID)
		if err != nil {
			return nil, ErrSignIn
		}
		u, err := s.Store.UserByID(c.UserID)
		if err != nil || u.Source != store.UserLocal || c.RPID != s.rpID || !bytes.Equal(userHandle, []byte(u.ID)) {
			return nil, ErrSignIn
		}
		used, who = c, u
		return owner{u: u, creds: []webauthn.Credential{toWebAuthn(c)}}, nil
	}
	_, cred, err := s.rp.ValidatePasskeyLogin(handler, cer.Data, parsed)
	if err != nil {
		return store.User{}, s.rejected(addr)
	}
	if err := s.Ceremonies.spend(cer); err != nil {
		return store.User{}, err
	}
	if err := s.recordUse(ctx, used, cred); err != nil {
		return store.User{}, err
	}
	return who, nil
}

// recordUse stores an assertion's counter and flags: a counter that
// didn't move forward (a cloned authenticator) refuses the sign-in.
func (s *Service) recordUse(ctx context.Context, c store.WebAuthnCredential, cred *webauthn.Credential) error {
	if cred.Authenticator.CloneWarning {
		return ErrSignIn
	}
	err := s.Store.UseWebAuthnCredential(ctx, c.ID, cred.Authenticator.SignCount, uint8(cred.Flags.ProtocolValue()))
	if errors.Is(err, store.ErrCounterReplay) {
		return ErrSignIn
	}
	return err
}

// BeginRegister starts creating a passkey for the local owner: the first
// one during setup (purpose PurposeSetup, bound to the setup state) or
// another (PurposeRegister, bound to the session).
func (s *Service) BeginRegister(ctx context.Context, addr netip.Addr, purpose Purpose, binding string) (Options, string, error) {
	if s.rp == nil {
		return nil, "", ErrNoPublicURL
	}
	u, err := s.Store.EnsureLocalUser(ctx)
	if err != nil {
		return nil, "", err
	}
	o, _, err := s.ownerWith(ctx, u)
	if err != nil {
		return nil, "", err
	}
	if all, err := s.Store.WebAuthnCredentials(ctx, u.ID); err != nil {
		return nil, "", err
	} else if len(all) >= MaxPasskeys {
		return nil, "", ErrTooManyPasskeys
	}
	opts, data, err := s.rp.BeginRegistration(o,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(webauthn.Credentials(o.creds).CredentialDescriptors()))
	if err != nil {
		return nil, "", err
	}
	id, err := s.Ceremonies.start(AddrKey(addr), ceremony{Purpose: purpose, Binding: binding, Data: *data})
	return opts, id, err
}

// maxPasskeyName bounds a passkey's name.
const maxPasskeyName = 60

// CleanPasskeyName trims a passkey's name; "" when it is unusable.
func CleanPasskeyName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxPasskeyName {
		return ""
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return name
}

// FinishRegister checks a new passkey (the browser's JSON, from addr)
// and returns it, ready to store; the caller stores it
// (store.CreateFirstPasskey during setup, AddWebAuthnCredential after).
func (s *Service) FinishRegister(ctx context.Context, addr netip.Addr, sealed string, purpose Purpose, binding string, body []byte, name string) (store.WebAuthnCredential, error) {
	if s.rp == nil {
		return store.WebAuthnCredential{}, ErrNoPublicURL
	}
	cer, err := s.openCeremony(addr, sealed, purpose, binding, "")
	if err != nil {
		return store.WebAuthnCredential{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		return store.WebAuthnCredential{}, s.rejected(addr)
	}
	u, err := s.Store.LocalUser(ctx)
	if err != nil {
		return store.WebAuthnCredential{}, err
	}
	o, _, err := s.ownerWith(ctx, u)
	if err != nil {
		return store.WebAuthnCredential{}, err
	}
	cred, err := s.rp.CreateCredential(o, cer.Data, parsed)
	if err != nil {
		return store.WebAuthnCredential{}, s.rejected(addr)
	}
	if err := s.Ceremonies.spend(cer); err != nil {
		return store.WebAuthnCredential{}, err
	}
	if name = CleanPasskeyName(name); name == "" {
		name = "Passkey"
	}
	var tr []string
	for _, t := range cred.Transport {
		tr = append(tr, string(t))
	}
	return store.WebAuthnCredential{
		RPID: s.rpID, CredentialID: cred.ID, PublicKey: cred.PublicKey, SignCount: cred.Authenticator.SignCount,
		Flags: uint8(cred.Flags.ProtocolValue()), Transports: tr, AAGUID: cred.Authenticator.AAGUID,
		AttestationType: cred.AttestationType, Name: name,
	}, nil
}

// BeginReauth starts a passkey assertion by the signed-in owner, for one
// operation, bound to the session.
func (s *Service) BeginReauth(ctx context.Context, addr netip.Addr, u store.User, sessionHash, op string) (Options, string, error) {
	if s.rp == nil {
		return nil, "", ErrNoPublicURL
	}
	o, _, err := s.ownerWith(ctx, u)
	if err != nil {
		return nil, "", err
	}
	if len(o.creds) == 0 {
		return nil, "", ErrNoPasskey
	}
	opts, data, err := s.rp.BeginLogin(o, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	id, err := s.Ceremonies.start(AddrKey(addr), ceremony{Purpose: PurposeReauth, Binding: sessionHash, Op: op, Data: *data})
	return opts, id, err
}

// FinishReauth checks the owner's assertion (from addr) for the
// operation.
func (s *Service) FinishReauth(ctx context.Context, addr netip.Addr, sealed string, u store.User, sessionHash, op string, body []byte) error {
	if s.rp == nil {
		return ErrNoPublicURL
	}
	cer, err := s.openCeremony(addr, sealed, PurposeReauth, sessionHash, op)
	if err != nil {
		return err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		return s.rejected(addr)
	}
	o, mine, err := s.ownerWith(ctx, u)
	if err != nil {
		return err
	}
	cred, err := s.rp.ValidateLogin(o, cer.Data, parsed)
	if err != nil {
		return s.rejected(addr)
	}
	for _, c := range mine {
		if bytes.Equal(c.CredentialID, cred.ID) {
			if err := s.Ceremonies.spend(cer); err != nil {
				return err
			}
			return s.recordUse(ctx, c, cred)
		}
	}
	return s.rejected(addr)
}

// CheckPassword checks the local owner's password from addr (known: a
// known browser, see KnownDevice), throttled: ErrThrottled (wait),
// ErrBusy (hashing is busy), ErrNoPassword (none is set) or ErrSignIn
// (wrong). It returns the owner.
func (s *Service) CheckPassword(ctx context.Context, addr netip.Addr, known bool, pw string) (store.User, error) {
	wait, everyone, err := s.Throttle.Wait(ctx, addr, known)
	if err != nil {
		return store.User{}, err
	}
	if wait > 0 {
		return store.User{}, &ErrThrottled{Wait: wait, Everyone: everyone}
	}
	u, err := s.Store.LocalUser(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return store.User{}, ErrNoPassword
	}
	if err != nil {
		return store.User{}, err
	}
	phc, err := s.Store.PasswordPHC(ctx, u.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.User{}, ErrNoPassword
	}
	if err != nil {
		return store.User{}, err
	}
	ok, err := s.Hasher.Verify(phc, pw)
	if err != nil {
		return store.User{}, err
	}
	if !ok {
		if err := s.Throttle.Failed(ctx, addr); err != nil {
			return store.User{}, err
		}
		return store.User{}, ErrSignIn
	}
	_ = s.Throttle.Succeeded(ctx, addr)
	return u, nil
}
