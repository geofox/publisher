package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

type bracketTranslator struct{}

func (bracketTranslator) Translate(_ context.Context, text, target string) (string, string, error) {
	return "[" + strings.ToUpper(text) + "]", "en", nil
}

// :translate translates each hand-made part on its own, so the translated
// draft's breaks fall at the translated parts' starts, media stay with
// their posts, and accounts' own thread positions (against the old text)
// are dropped.
func TestTranslateDraftKeepsPartsAndBreaks(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	d, _ := e.newDraft(`{"post":{"content":{"text":"First part here. Second part.","breaks":[17]}}}`)
	e.api.Translator = bracketTranslator{}
	r := e.do(req{method: "POST", path: "/v1/drafts/" + d.ID + ":translate", body: `{"target_lang":"fr"}`})
	wantStatus(t, r, 201)
	var tr v1DraftBody
	r.json(&tr)
	var p v1CreatePostReq
	_ = json.Unmarshal(tr.Post, &p)
	if got, want := *p.Content.Text, "[FIRST PART HERE.] [SECOND PART.]"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	if want := []int{19}; !slices.Equal(p.Content.Breaks, want) {
		t.Errorf("breaks = %v, want %v", p.Content.Breaks, want)
	}
}

func TestTranslatePartsMovesMedia(t *testing.T) {
	text, breaks, starts, err := translateParts(context.Background(), bracketTranslator{}, "ab\n\ncd\n\nef", []int{4, 8}, "fr")
	if err != nil {
		t.Fatal(err)
	}
	if text != "[AB]\n\n[CD]\n\n[EF]" || !slices.Equal(breaks, []int{6, 12}) || !slices.Equal(starts, []int{0, 6, 12}) {
		t.Errorf("got %q %v %v", text, breaks, starts)
	}
}
