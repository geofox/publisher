package nostr

import (
	"context"
	"time"

	gonostr "fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/destination"
)

// Account configuration and secret names.
const (
	ConfigBootstrapRelay = "bootstrap_relay" // the home relay; NIP-65 lookups start here
	ConfigFallbackRelays = "fallback_relays"
	ConfigPrimaryRelays  = "primary_relays"
	ConfigPrimaryFanout  = "primary_fanout"
	ConfigPOWDefault     = "pow_default"
	ConfigPOWMax         = "pow_max"
	// ConfigSyncRelaysSeed is the relay-sync list a new account starts
	// with.
	ConfigSyncRelaysSeed = "sync_relays_seed"
	SecretKey            = "secret_key" // hex
)

// Default proof-of-work difficulties.
const (
	DefaultPOW    = 16
	DefaultPOWMax = 28
)

// Open implements destination.Driver.
//
// The account's own relays (in its config, and the NIP-65 ones its profile
// lists) are dialed through the host's NetAccount network; relays named by
// content (pasted links) through its NetContent one.
func (Driver) Open(_ context.Context, req destination.OpenRequest) (destination.Account, error) {
	keyHex := req.Secrets[SecretKey]
	if keyHex == "" {
		return nil, destination.InvalidField(SecretKey, "required")
	}
	sk, err := ParseSecretKey(keyHex)
	if err != nil {
		return nil, destination.InvalidField(SecretKey, err.Error())
	}
	home, err := destination.ConfigString(req.Config, ConfigBootstrapRelay)
	if err != nil {
		return nil, destination.InvalidField(ConfigBootstrapRelay, err.Error())
	}
	if home == "" {
		return nil, destination.InvalidField(ConfigBootstrapRelay, "required")
	}
	fallback, err := destination.ConfigStrings(req.Config, ConfigFallbackRelays)
	if err != nil {
		return nil, destination.InvalidField(ConfigFallbackRelays, err.Error())
	}
	primary, err := destination.ConfigStrings(req.Config, ConfigPrimaryRelays)
	if err != nil {
		return nil, destination.InvalidField(ConfigPrimaryRelays, err.Error())
	}
	if len(primary) == 0 {
		primary = []string{home}
	}
	fanout, _, err := destination.ConfigBool(req.Config, ConfigPrimaryFanout)
	if err != nil {
		return nil, destination.InvalidField(ConfigPrimaryFanout, err.Error())
	}
	powDefault, ok, err := destination.ConfigInt(req.Config, ConfigPOWDefault)
	if err != nil {
		return nil, destination.InvalidField(ConfigPOWDefault, err.Error())
	}
	if !ok {
		powDefault = DefaultPOW
	}
	powMax, ok, err := destination.ConfigInt(req.Config, ConfigPOWMax)
	if err != nil {
		return nil, destination.InvalidField(ConfigPOWMax, err.Error())
	}
	if !ok {
		powMax = DefaultPOWMax
	}
	if powDefault < 0 {
		return nil, destination.InvalidField(ConfigPOWDefault, "must not be negative")
	}
	if powMax < 0 {
		return nil, destination.InvalidField(ConfigPOWMax, "must not be negative")
	}
	return &Account{HTTP: req.Host.Net(destination.NetAccount).Client(10 * time.Second), P: New(Config{
		NSEC:                 sk,
		OwnerPubkey:          gonostr.GetPublicKey(sk),
		NIP65BootstrapRelay:  home,
		FallbackRelays:       fallback,
		POWDifficultyDefault: powDefault,
		POWDifficultyMax:     powMax,
		POWTimeout:           req.Tuning.POWTimeout,
		RelayCacheTTL:        req.Tuning.RelayCacheTTL,
		PublishTimeout:       req.Tuning.PublishTimeout,
		PrimaryFanout:        fanout,
		PrimaryRelays:        primary,
		ContentClient:        req.Host.Net(destination.NetContent).Client(0),
		RelayClient:          req.Host.Net(destination.NetAccount).Client(0),
	})}, nil
}
