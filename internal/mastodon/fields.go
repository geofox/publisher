package mastodon

import "github.com/geofox/publisher/internal/destination"

// MethodToken connects with an access token the owner made on their
// server.
const MethodToken = "token"

func authMethods() []destination.AuthMethod {
	return []destination.AuthMethod{{
		ID:             MethodOAuth,
		Flow:           destination.FlowRedirect,
		Label:          "Sign in at your server",
		NeedsPublicURL: true,
		Help: "Publisher registers itself on your Mastodon server and sends you there to sign in and allow it " +
			"to read your account and post for you. Needs PUBLIC_URL, where the server sends you back.",
	}, {
		ID:    MethodToken,
		Flow:  destination.FlowFields,
		Label: "Access token",
		Help: "On your Mastodon server: Preferences, Development, New application. Give it the scopes " +
			"read:accounts, read:search, read:statuses, write:media and write:statuses, save it, " +
			"then copy “Your access token”.",
		Fields: []destination.FieldSpec{
			{Key: SecretAccessToken, Label: "Access token", Type: destination.FieldString, Secret: true, Required: true,
				MaxLength: 512},
		},
	}}
}

func configFields() []destination.FieldSpec {
	return []destination.FieldSpec{
		{Key: ConfigInstanceURL, Label: "Server address", Type: destination.FieldURL, URL: &destination.HTTPSOrigin,
			Audience: true, Required: true, Placeholder: "https://mastodon.social"},
	}
}
