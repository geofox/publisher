package threads

import "github.com/geofox/publisher/internal/destination"

// MethodToken connects with a long-lived access token.
const MethodToken = "token"

func authMethods() []destination.AuthMethod {
	return []destination.AuthMethod{{
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
