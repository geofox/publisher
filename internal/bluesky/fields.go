package bluesky

import "github.com/geofox/publisher/internal/destination"

// MethodAppPassword connects with a handle (or DID, or email) and an app
// password.
const MethodAppPassword = "app_password"

func authMethods() []destination.AuthMethod {
	return []destination.AuthMethod{{
		ID:    MethodAppPassword,
		Flow:  destination.FlowFields,
		Label: "App password",
		Help: "Make one in Bluesky under Settings, Privacy and security, App passwords. " +
			"It can post but can't change your account or password.",
		HelpURL: "https://bsky.app/settings/app-passwords",
		Fields: []destination.FieldSpec{
			{Key: ConfigIdentifier, Label: "Handle, DID or email", Type: destination.FieldString, Required: true,
				Placeholder: "you.bsky.social", MaxLength: 512,
				Help: "With an email, give the PDS address too (under Advanced)."},
			{Key: SecretAppPassword, Label: "App password", Type: destination.FieldString, Secret: true, Required: true,
				Placeholder: "xxxx-xxxx-xxxx-xxxx", MaxLength: 256},
		},
	}}
}

func configFields() []destination.FieldSpec {
	return []destination.FieldSpec{
		{Key: ConfigPDSURL, Label: "PDS address", Type: destination.FieldURL, URL: &destination.HTTPSOrigin,
			Audience: true, Advanced: true, Placeholder: "https://bsky.social",
			Help: "The server your account lives on. Found from your handle when left empty."},
	}
}
