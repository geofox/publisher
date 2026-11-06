package resolve

import (
	"reflect"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// Every field a driver returns reaches /v1/resolve's shape.
func TestFromResolvedCarriesEveryField(t *testing.T) {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	rs := destination.ResolvedSource{
		Ref: destination.SourceRef{Type: "x", URI: "at://u", CID: "c", ReplyRootURI: "at://r", ReplyRootCID: "rc",
			LocalID: "9", EventID: "e", Author: "a", RelayHints: []string{"wss://h"}, EventKind: 1,
			ThreadRootID: "t", Mentions: []string{"m"}},
		Preview: destination.SourcePreview{AuthorName: "N", AuthorHandle: "@h", Text: "hi",
			Media: []destination.SourceMedia{{URL: "https://m", Alt: "alt", Video: true}}, CreatedAt: at, WebURL: "https://w"},
		Actions: destination.SourceActions{
			Reply:  destination.ActionAllowed{Allowed: true},
			Quote:  destination.ActionAllowed{Reason: "no"},
			Repost: destination.ActionAllowed{Allowed: true, Reason: "note"},
		},
	}
	got := FromResolved(rs)
	want := &SourceRef{
		Platform: "x",
		Ref: PlatformRef{URI: "at://u", CID: "c", ReplyRootURI: "at://r", ReplyRootCID: "rc", LocalID: "9",
			EventID: "e", Author: "a", RelayHints: []string{"wss://h"}, Kind: 1, ThreadRootID: "t", Mentions: []string{"m"}},
		Preview: Preview{AuthorName: "N", AuthorHandle: "@h", Text: "hi",
			Media: []Media{{URL: "https://m", Alt: "alt", Video: true}}, CreatedAt: at, WebURL: "https://w"},
		Caps: Caps{Reply: Cap{Allowed: true}, Quote: Cap{Reason: "no"}, Repost: Cap{Allowed: true, Reason: "note"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%+v\nwant\n%+v", got, want)
	}
}
