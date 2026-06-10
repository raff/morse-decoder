package morse

import "strings"

// Decoder accumulates Symbols and produces ASCII text.
// Feed it symbols in order; call Flush() at the end to get the result.
type Decoder struct {
	current strings.Builder // dot/dash sequence for the character in progress
	output  strings.Builder
}

// Feed processes one Symbol. IntraGap is a no-op (dots and dashes accumulate
// in current); CharGap and WordGap flush the buffered character.
func (d *Decoder) Feed(sym Symbol) {
	switch sym.Type {
	case SymDot:
		d.current.WriteByte('.')
	case SymDash:
		d.current.WriteByte('-')
	case SymIntraGap:
		// nothing: elements within a character are separated only by timing
	case SymCharGap:
		d.flushChar()
	case SymWordGap:
		d.flushChar()
		d.output.WriteByte(' ')
	}
}

// Flush finalises any buffered character and returns the full decoded string.
func (d *Decoder) Flush() string {
	d.flushChar()
	return strings.TrimSpace(d.output.String())
}

func (d *Decoder) flushChar() {
	code := d.current.String()
	d.current.Reset()
	if code == "" {
		return
	}
	d.output.WriteRune(Lookup(code))
}
