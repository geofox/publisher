package resolve

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

type fakeResolver struct {
	rs  destination.ResolvedSource
	err error
}

func (f fakeResolver) ResolveSource(context.Context, string) (destination.ResolvedSource, error) {
	return f.rs, f.err
}

// Every field a driver returns reaches /v1/resolve's shape.
func TestAccountAdapterCarriesEveryField(t *testing.T) {
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
	got, err := AccountAdapter{R: fakeResolver{rs: rs}}.ResolveSource(context.Background(), "in")
	if err != nil {
		t.Fatal(err)
	}
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

// The driver's error comes back unchanged, so the API can still tell a
// network failure from a missing post.
func TestAccountAdapterKeepsErrors(t *testing.T) {
	he := &destination.HTTPError{Op: "search", StatusCode: 503}
	_, err := AccountAdapter{R: fakeResolver{err: he}}.ResolveSource(context.Background(), "in")
	var got *destination.HTTPError
	if !errors.As(err, &got) || got.StatusCode != 503 {
		t.Fatalf("err = %v, want the HTTP error", err)
	}
}
