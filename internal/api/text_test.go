package api

import "testing"

// Names that would show as something else, or as nothing, are refused;
// ordinary names in any script, with emoji (joiners included), pass.
func TestNameProblem(t *testing.T) {
	for _, s := range []string{"Personal", "Mastodon (work)", "Caf\u00e9", "\u65e5\u672c\u8a9e", "\u0395\u03bb\u03bb",
		"\u05e2\u05d1\u05e8\u05d9\u05ea", "\U0001F469\u200d\U0001F4BB dev", "\U0001F1EB\U0001F1F7", "a b", "a\u00a0b", "\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645"} {
		if p := nameProblem(s); p != "" {
			t.Errorf("%q refused: %s", s, p)
		}
	}
	for _, s := range []string{
		"Personal \u202ekrow\u202c", // right-to-left override
		"\u2067Work\u2069",          // isolates
		"\u200b",                    // zero-width space alone
		"\u200e\u200f\u2060\ufeff",  // marks, word joiner, byte order mark
		"a\u2028b", "a\u2029b",      // line and paragraph separators
		"a\nb", "a\x7fb", // controls
		"\ue000x",            // private use
		"x\ufdd0", "x\uffff", // noncharacters
		" ", "",
		"bad\xffutf8",
	} {
		if nameProblem(s) == "" {
			t.Errorf("%q accepted", s)
		}
	}
}
