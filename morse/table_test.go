package morse

import "testing"

func TestDecode(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		// Letters and digits.
		{".-", "A"},
		{"-----", "0"},
		// Punctuation that is not a prosign stays punctuation.
		{".-.-.-", "."},
		{"-..-.", "/"},
		// Colliding codes render the combined prosign/punctuation form.
		{".-...", "<AS/&>"},
		{".-.-.", "<AR/+>"},
		{"-...-", "<BT/=>"},
		{"-.--.", "<KN/(>"},
		// Unambiguous prosigns.
		{"...-.-", "<SK>"},
		{"...---...", "<SOS>"},
		{"........", "<HH>"},
		{"-.-..-..", "<CL>"},
		// Unknown.
		{"........-.....", "?"},
		{"", "?"},
	}
	for _, c := range cases {
		if got := Decode(c.code); got != c.want {
			t.Errorf("Decode(%q) = %q, want %q", c.code, got, c.want)
		}
	}
}

// A prosign sent as a single run decodes to the prosign; the same letters sent
// with a character gap between them decode as separate characters.
func TestProsignVsSeparateChars(t *testing.T) {
	var d Decoder
	for _, s := range ".-..." { // A then S, no SymCharGap between them
		if s == '.' {
			d.Feed(Symbol{Type: SymDot})
		} else {
			d.Feed(Symbol{Type: SymDash})
		}
	}
	if got := d.Flush(); got != "<AS/&>" {
		t.Errorf("run-together = %q, want %q", got, "<AS/&>")
	}

	var d2 Decoder
	feed := func(code string) {
		for _, s := range code {
			if s == '.' {
				d2.Feed(Symbol{Type: SymDot})
			} else {
				d2.Feed(Symbol{Type: SymDash})
			}
		}
	}
	feed(".-")
	d2.Feed(Symbol{Type: SymCharGap})
	feed("...")
	if got := d2.Flush(); got != "AS" {
		t.Errorf("separated = %q, want %q", got, "AS")
	}
}
