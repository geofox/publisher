package nostr

import (
	"errors"
	"fmt"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/destination"
)

// Defaults of the Nostr environment variables: large public relays. The
// home relay gets every post and holds the relay list (NIP-65), so it is
// a general relay, not an index of profiles and relay lists only.
const (
	defaultBootstrapRelay = "wss://relay.damus.io"
	defaultFallbackRelays = "wss://nos.lol,wss://relay.damus.io,wss://relay.primal.net"
	defaultSyncRelays     = "wss://nos.lol,wss://relay.damus.io,wss://nostr.wine,wss://nostr.land,wss://relay.nostr.band,wss://purplepag.es,wss://relay.snort.social,wss://nostr.mom"
)

var _ destination.EnvSource = Driver{}

// EnvVars implements destination.EnvSource.
func (Driver) EnvVars() []destination.EnvVar {
	vars := []destination.EnvVar{{Name: "NSEC_HEX", Required: true, Secret: true}}
	for _, n := range []string{"OWNER_PUBKEY", "NIP65_BOOTSTRAP_RELAY", "FALLBACK_RELAYS", "PRIMARY_RELAYS",
		"NOSTR_PRIMARY_FANOUT", "POW_DIFFICULTY_DEFAULT", "POW_DIFFICULTY_MAX", "SYNC_RELAYS"} {
		vars = append(vars, destination.EnvVar{Name: n})
	}
	return vars
}

// FromEnv implements destination.EnvSource: NSEC_HEX configures one
// account. OWNER_PUBKEY is optional (it is derived from the key) but must
// match when set.
func (Driver) FromEnv(getenv func(string) string) ([]destination.EnvAccount, error) {
	nsec := destination.EnvString(getenv, "NSEC_HEX", "")
	pub := destination.EnvString(getenv, "OWNER_PUBKEY", "")
	if nsec == "" {
		if pub != "" {
			return nil, errors.New("OWNER_PUBKEY is set but NSEC_HEX is not: set NSEC_HEX (the public key is derived from it)")
		}
		return nil, nil
	}
	var errs []error
	sk, err := ParseSecretKey(nsec)
	if err != nil {
		errs = append(errs, fmt.Errorf("NSEC_HEX: %w", err))
	} else if pub != "" {
		pk, err := gonostr.PubKeyFromHex(pub)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("OWNER_PUBKEY: %w", err))
		case gonostr.GetPublicKey(sk) != pk:
			errs = append(errs, fmt.Errorf("OWNER_PUBKEY (%s) does not match the pubkey derived from NSEC_HEX (%s)",
				pk.Hex(), gonostr.GetPublicKey(sk).Hex()))
		}
	}
	home := destination.EnvString(getenv, "NIP65_BOOTSTRAP_RELAY", defaultBootstrapRelay)
	primary := destination.EnvList(getenv, "PRIMARY_RELAYS", home)
	if len(primary) == 0 {
		primary = []string{home}
	}
	fanout, err := destination.EnvBool(getenv, "NOSTR_PRIMARY_FANOUT", false)
	if err != nil {
		errs = append(errs, err)
	}
	powDefault, err := destination.EnvInt(getenv, "POW_DIFFICULTY_DEFAULT", DefaultPOW)
	if err != nil {
		errs = append(errs, err)
	} else if powDefault < 0 {
		errs = append(errs, errors.New("POW_DIFFICULTY_DEFAULT must not be negative (0 turns proof of work off)"))
	}
	powMax, err := destination.EnvInt(getenv, "POW_DIFFICULTY_MAX", DefaultPOWMax)
	if err != nil {
		errs = append(errs, err)
	} else if powMax < 0 {
		errs = append(errs, errors.New("POW_DIFFICULTY_MAX must not be negative (0 turns proof of work off)"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return []destination.EnvAccount{{
		Key: destination.EnvKeyDefault,
		Config: map[string]any{
			ConfigBootstrapRelay: home,
			ConfigFallbackRelays: destination.EnvList(getenv, "FALLBACK_RELAYS", defaultFallbackRelays),
			ConfigPrimaryRelays:  primary,
			ConfigPrimaryFanout:  fanout,
			ConfigPOWDefault:     powDefault,
			ConfigPOWMax:         powMax,
			ConfigSyncRelaysSeed: destination.EnvList(getenv, "SYNC_RELAYS", defaultSyncRelays),
		},
		Secrets: map[string]string{SecretKey: nsec},
	}}, nil
}
