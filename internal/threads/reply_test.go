package threads

import (
	"net/url"
	"testing"
)

func TestAddReplyTo(t *testing.T) {
	v := url.Values{}
	(&Client{}).addReplyTo(v, "999")
	if v.Get("reply_to_id") != "999" {
		t.Errorf("reply_to_id = %q, want 999", v.Get("reply_to_id"))
	}
}

func TestAddReplyToEmpty(t *testing.T) {
	v := url.Values{}
	(&Client{}).addReplyTo(v, "")
	if _, ok := v["reply_to_id"]; ok {
		t.Errorf("empty reply id must not set the param: %v", v)
	}
}
