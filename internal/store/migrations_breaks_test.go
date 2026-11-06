package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConvertBreakMarkers(t *testing.T) {
	in := `{"content":{"text":"one\n---\ntwo\n---\nthree","media":[{"id":"med_a"},{"id":"med_b","part":2},{"id":"med_c","part":0}]},` +
		`"targets":[{"account":"acc_1"},{"account":"acc_2","content":{"text":"x\n---\ny"}},{"account":"acc_3","content":{"text":"plain"}}]}`
	out, ok := ConvertBreakMarkers(in)
	if !ok {
		t.Fatal("nothing converted")
	}
	var got struct {
		Content struct {
			Text   string           `json:"text"`
			Breaks []int            `json:"breaks"`
			Media  []map[string]any `json:"media"`
		} `json:"content"`
		Targets []struct {
			Content *struct {
				Text string `json:"text"`
			} `json:"content"`
			Thread *struct {
				Breaks  []int `json:"breaks"`
				MediaAt []int `json:"media_at"`
			} `json:"thread"`
		} `json:"targets"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Content.Text != "one\ntwo\nthree" || !reflect.DeepEqual(got.Content.Breaks, []int{4, 8}) {
		t.Fatalf("content %q %v", got.Content.Text, got.Content.Breaks)
	}
	for _, m := range got.Content.Media {
		if _, has := m["part"]; has {
			t.Fatalf("part kept: %v", m)
		}
	}
	if got.Content.Media[1]["at"] != float64(8) || got.Content.Media[0]["at"] != nil || got.Content.Media[2]["at"] != nil {
		t.Fatalf("media %v", got.Content.Media)
	}
	if got.Targets[0].Thread != nil {
		t.Fatal("a target on the shared text got its own thread")
	}
	t1 := got.Targets[1]
	if t1.Content.Text != "x\ny" || !reflect.DeepEqual(t1.Thread.Breaks, []int{2}) || !reflect.DeepEqual(t1.Thread.MediaAt, []int{0, 2, 0}) {
		t.Fatalf("own text target %+v %+v", t1.Content, t1.Thread)
	}
	if t2 := got.Targets[2]; t2.Content.Text != "plain" || !reflect.DeepEqual(t2.Thread.MediaAt, []int{0, 0, 0}) {
		t.Fatalf("plain own text %+v %+v", t2.Content, t2.Thread)
	}
	if _, ok := ConvertBreakMarkers(`{"content":{"text":"no markers"},"targets":[]}`); ok {
		t.Fatal("converted a body with nothing to convert")
	}
	if _, ok := ConvertBreakMarkers(`not json`); ok {
		t.Fatal("converted a non-JSON body")
	}
}

func TestMigrateDraftBreaks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := openWith(t, path, migrations[:8])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	mustExec(t, db, `INSERT INTO drafts (id, title, master_text, created_at, updated_at, spec_json, body_json, version)
		VALUES ('d1', 't', 'a\n---\nb', ?, ?, '{}', ?, 3), ('d2', 't', 'plain', ?, ?, '{}', ?, 5)`,
		now, now, `{"content":{"text":"a\n---\nb"},"targets":[]}`, now, now, `{"content":{"text":"plain"},"targets":[]}`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var body, text string
	var version int
	if err := s.sql.QueryRow(`SELECT body_json, master_text, version FROM drafts WHERE id = 'd1'`).Scan(&body, &text, &version); err != nil {
		t.Fatal(err)
	}
	if body != `{"content":{"breaks":[2],"text":"a\nb"},"targets":[]}` || text != "a\nb" || version != 4 {
		t.Fatalf("d1 = %s %q v%d", body, text, version)
	}
	if err := s.sql.QueryRow(`SELECT body_json, version FROM drafts WHERE id = 'd2'`).Scan(&body, &version); err != nil {
		t.Fatal(err)
	}
	if body != `{"content":{"text":"plain"},"targets":[]}` || version != 5 {
		t.Fatalf("d2 changed: %s v%d", body, version)
	}
}

// A marker that made no break is taken out too: it would post as text now.
func TestConvertBreakMarkersDropsEdgeMarkers(t *testing.T) {
	for in, want := range map[string]string{
		`{"content":{"text":"Intro\n---"}}`: "Intro\n", // the post trims it
		`{"content":{"text":"---\nIntro"}}`: "Intro",
	} {
		out, ok := ConvertBreakMarkers(in)
		var p struct{ Content struct{ Text string } }
		if err := json.Unmarshal([]byte(out), &p); err != nil || !ok || p.Content.Text != want {
			t.Errorf("%s → %v %s", in, ok, out)
		}
		if _, again := ConvertBreakMarkers(out); again {
			t.Errorf("%s converted twice", in)
		}
	}
	out, _ := ConvertBreakMarkers(`{"content":{"text":"a"},"targets":[{"account":"x","content":{"text":"own\n---"}}]}`)
	if !strings.Contains(out, `"text":"own\n"`) {
		t.Errorf("own text kept its marker: %s", out)
	}
}

// Converting keeps numbers as written and merges into a thread object the
// body already has.
func TestConvertBreakMarkersKeepsTheRest(t *testing.T) {
	out, ok := ConvertBreakMarkers(`{"content":{"text":"a\n---\nb"},"big":9007199254740993,` +
		`"targets":[{"account":"x","content":{"text":"o\n---\np"},"thread":{"note":1}}]}`)
	if !ok || !strings.Contains(out, `"big":9007199254740993`) {
		t.Fatalf("number changed: %s", out)
	}
	if !strings.Contains(out, `"note":1`) || !strings.Contains(out, `"breaks":[2]`) {
		t.Fatalf("thread not merged: %s", out)
	}
}
