package api

import (
	"unicode"
	"unicode/utf8"
)

// nameProblem says what is wrong with a name the owner gives something
// shown in lists and alerts (an account's label, a token's name), or ""
// when it is fine. Besides control characters it refuses the invisible
// ones that can make one name pass for another: format characters (bidi
// overrides and isolates, zero-width spaces and joiners, byte order
// marks), line and paragraph separators, private-use characters,
// surrogates and noncharacters. A zero-width joiner or non-joiner may
// follow a visible character (emoji sequences, Persian, Indic scripts).
// It needs one visible character.
func nameProblem(s string) string {
	if !utf8.ValidString(s) {
		return "not valid UTF-8"
	}
	visible := false
	prev := rune(0)
	for _, r := range s {
		joiner := r == '\u200c' || r == '\u200d' // ZWNJ, ZWJ: needed by some scripts and emoji sequences
		switch {
		case joiner && unicode.IsGraphic(prev) && !unicode.IsSpace(prev):
		case unicode.IsControl(r):
			return "no control characters"
		case unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Co, unicode.Cs) || isNoncharacter(r):
			return "no invisible or formatting characters (such as direction marks or zero-width spaces)"
		case unicode.IsGraphic(r) && !unicode.IsSpace(r):
			visible = true
		}
		prev = r
	}
	if !visible {
		return "at least one visible character"
	}
	return ""
}

// isNoncharacter reports Unicode's noncharacters: U+FDD0–U+FDEF and the
// last two code points of every plane.
func isNoncharacter(r rune) bool {
	return r >= 0xFDD0 && r <= 0xFDEF || r&0xFFFE == 0xFFFE
}
