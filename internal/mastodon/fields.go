package mastodon

import "github.com/geofox/publisher/internal/destination"

// MethodToken connects with an access token the owner made on their
// server.
const MethodToken = "token"

func authMethods() []destination.AuthMethod {
	return []destination.AuthMethod{{
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
