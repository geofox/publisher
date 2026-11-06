package destination

// Manifest describes a destination type. It is static: the same for every
// account of the type. Capabilities refine Defaults per account.
type Manifest struct {
	// Contract is the ContractVersion the driver was written against.
	Contract int `json:"contract"`
	// Type is the stable identifier ("mastodon"). Lowercase, [a-z0-9_].
	Type string `json:"type"`
	// Name is the display name ("Mastodon").
	Name string `json:"name"`
	// Kinds lists what the type can publish.
	Kinds []Kind `json:"kinds"`
	// Options are the type's own settings, beyond the common fields of
	// PublishRequest.
	Options []OptionSpec `json:"options,omitempty"`
	// Defaults are the capabilities assumed before (or without) asking a
	// server.
	Defaults Capabilities `json:"defaults"`
}

// Option returns the option spec with the given key.
func (m Manifest) Option(key string) (OptionSpec, bool) {
	for _, o := range m.Options {
		if o.Key == key {
			return o, true
		}
	}
	return OptionSpec{}, false
}

// Capabilities is what an account can do.
type Capabilities struct {
	Text      TextCaps      `json:"text"`
	Title     TitleCaps     `json:"title"`
	Threading ThreadingCaps `json:"threading"`
	Media     MediaCaps     `json:"media"`
	// LinkPreview says who builds link previews: "none", "server" (the
	// network unfurls links itself) or "client" (we must attach a card).
	LinkPreview    string `json:"link_preview"`
	ContentWarning bool   `json:"content_warning"`
	Sensitive      bool   `json:"sensitive"`
	// Languages is how many language tags a post can carry (0 = none).
	Languages int `json:"languages"`
	// Visibility lists the accepted visibility values; empty = not
	// supported.
	Visibility   []string        `json:"visibility,omitempty"`
	Interactions InteractionCaps `json:"interactions"`
	Idempotent   IdempotencyMode `json:"idempotent"`
	// IdempotencyKeyTTLSecs is how long the server remembers a key, for
	// IdempotentKey (0 = indefinitely). A repeat after it can duplicate.
	IdempotencyKeyTTLSecs int64 `json:"idempotency_key_ttl_secs,omitempty"`
	// CommitMarks: Publish calls BeforeCommit right before the request that
	// publishes the item, so a restart before it is known to have sent
	// nothing. Internal: not part of the API.
	CommitMarks bool `json:"-"`
}

// Link preview modes.
const (
	LinkPreviewNone   = "none"
	LinkPreviewServer = "server"
	LinkPreviewClient = "client"
)

// TextCaps describes a note's text.
type TextCaps struct {
	// MaxLength is the limit, measured per Counting. 0 = unlimited.
	MaxLength int      `json:"max_length"`
	Counting  Counting `json:"counting"`
	// MaxLinks caps the distinct URLs in one post; 0 = no cap.
	MaxLinks int `json:"max_links,omitempty"`
}

// TitleCaps describes an article title.
type TitleCaps struct {
	Supported bool `json:"supported"`
	Required  bool `json:"required"`
	MaxLength int  `json:"max_length,omitempty"`
}

// ThreadingCaps describes reply chains of the account's own posts.
type ThreadingCaps struct {
	Supported bool `json:"supported"`
	// MaxItems caps a thread's length; 0 = unlimited.
	MaxItems int `json:"max_items,omitempty"`
}

// InteractionCaps lists the native interactions with external posts.
type InteractionCaps struct {
	Reply  bool `json:"reply"`
	Quote  bool `json:"quote"`
	Repost bool `json:"repost"`
	// QuoteVideo: a native quote can carry a video.
	QuoteVideo bool `json:"quote_video"`
}

// IdempotencyMode says how a repeated publish of the same item behaves.
type IdempotencyMode string

const (
	// IdempotentNone: a repeat creates a duplicate.
	IdempotentNone IdempotencyMode = "none"
	// IdempotentKey: the server drops a repeat carrying the same
	// IdempotencyKey (possibly only for a limited time).
	IdempotentKey IdempotencyMode = "key"
	// IdempotentNatural: re-sending is harmless by construction (a signed
	// Nostr event has one id).
	IdempotentNatural IdempotencyMode = "natural"
)

// MediaCaps describes attachments.
type MediaCaps struct {
	// Transport is how media reaches the network: "bytes" (we upload them)
	// or "url" (the network fetches a public URL).
	Transport string `json:"transport"`
	Images    Limit  `json:"images"`
	Video     Limit  `json:"video"`
	// MixImagesVideo: images and a video may share one item.
	MixImagesVideo bool `json:"mix_images_video"`
	// AltText is supported, up to AltMaxLength characters (0 = no limit).
	AltText      bool `json:"alt_text"`
	AltMaxLength int  `json:"alt_max_length,omitempty"`
}

// Media transports.
const (
	TransportBytes = "bytes"
	TransportURL   = "url"
)

// Limit bounds one kind of media per item. Zero values mean "no limit".
type Limit struct {
	// MaxCount per item. 0 = unlimited; set Supported false to forbid.
	Supported bool  `json:"supported"`
	MaxCount  int   `json:"max_count,omitempty"`
	MaxBytes  int64 `json:"max_bytes,omitempty"`
	// MaxPixels is width*height.
	MaxPixels int64 `json:"max_pixels,omitempty"`
	// Mimes accepted as-is; empty = any of the kind.
	Mimes []string `json:"mimes,omitempty"`
	// MaxAspect is the widest long-edge:short-edge ratio an image may have
	// (0 = any). No re-encode fixes a wider one.
	MaxAspect float64 `json:"max_aspect,omitempty"`
	// MaxDurationSecs is a hard limit for video; AdvisoryDurationSecs a
	// length past which the network may degrade playback.
	MaxDurationSecs      int64 `json:"max_duration_secs,omitempty"`
	AdvisoryDurationSecs int64 `json:"advisory_duration_secs,omitempty"`
}

// OptionSpec declares one destination-specific option, so a UI can render it
// and the core can validate it without knowing the destination.
type OptionSpec struct {
	Key   string      `json:"key"`
	Type  OptionType  `json:"type"`
	Enum  []EnumValue `json:"enum,omitempty"`
	Label string      `json:"label"`
	Help  string      `json:"help,omitempty"`
	// Min and Max bound an int option.
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
}

// OptionType is an option's value type.
type OptionType string

const (
	OptionBool   OptionType = "bool"
	OptionString OptionType = "string"
	OptionInt    OptionType = "int"
	OptionEnum   OptionType = "enum"
	// OptionEnumList is a set of Enum values.
	OptionEnumList OptionType = "enum_list"
)

// EnumValue is one choice of an enum option. "" is a valid value meaning
// "default".
type EnumValue struct {
	Value string `json:"value"`
	Label string `json:"label"`
}
