package threads

import "github.com/geofox/publisher/internal/destination"

// Ways to connect.
const (
	// MethodOAuth connects by signing in at Threads with the server's
	// Meta app (InstanceAppID, InstanceAppSecret).
	MethodOAuth = "oauth"
	// MethodToken connects with a long-lived access token.
	MethodToken = "token"
)

// The server's Meta app: instance settings threads.app_id and
// threads.app_secret.
const (
	InstanceAppID     = "app_id"
	InstanceAppSecret = "app_secret"
)

// GuideURL is Meta's guide to making the app.
const GuideURL = "https://developers.facebook.com/documentation/threads/get-started"

func authMethods() []destination.AuthMethod {
	return []destination.AuthMethod{{
		ID:      MethodOAuth,
		Flow:    destination.FlowRedirect,
		Label:   "Sign in at Threads",
		HelpURL: GuideURL,
		Help: "Uses the server's Meta app (Settings, Integrations, Threads app): you sign in at Threads and allow it to " +
			"read your profile, post and reply for you. Needs PUBLIC_URL at an https address, which Meta sends you back to. " +
			"Publisher renews the token; Meta's grant lasts 90 days, then you sign in again.",
		NeedsInstanceConfig: []string{InstanceAppID, InstanceAppSecret},
		NeedsPublicURL:      true,
	}, {
		ID:    MethodToken,
		Flow:  destination.FlowFields,
		Label: "Long-lived access token",
		Help: "A long-lived Threads token from your Meta app. It lasts 60 days; " +
			"Publisher refreshes it before it runs out.",
		Fields: []destination.FieldSpec{
			{Key: SecretAccessToken, Label: "Access token", Type: destination.FieldString, Secret: true, Required: true,
				MaxLength: 1024},
		},
	}}
}

func configFields() []destination.FieldSpec {
	return []destination.FieldSpec{
		{Key: ConfigUserID, Label: "Threads user id", Type: destination.FieldString, Default: "me",
			Pattern: `me|[0-9]{1,32}`, Advanced: true,
			Help: "“me” is the token's own account."},
	}
}

// instanceConfig is the server's Meta app, set once in Settings.
func instanceConfig() []destination.FieldSpec {
	return []destination.FieldSpec{
		{Key: InstanceAppID, Label: "Threads app ID", Type: destination.FieldString, Pattern: `[0-9]{1,32}`,
			Placeholder: "1234567890123456",
			Help: "In your Meta app's dashboard: App settings, Basic, Threads App ID. A Meta app has two IDs: " +
				"use the Threads one."},
		{Key: InstanceAppSecret, Label: "Threads app secret", Type: destination.FieldString, Secret: true, MaxLength: 200,
			Help: "App settings, Basic, Threads App secret (the one paired with the Threads App ID). " +
				"Kept sealed; sent only to Threads."},
	}
}
