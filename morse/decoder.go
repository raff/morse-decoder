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
		// Idempotent: only add a space if the output doesn't already end
		// with one, so feeding SymWordGap twice for the same gap (e.g. an
		// early speculative peek followed by the real completion event)
		// doesn't produce a double space.
		if s := d.output.String(); len(s) > 0 && s[len(s)-1] != ' ' {
			d.output.WriteByte(' ')
		}
	}
}

// Peek returns the decoded output so far, flushing any character currently
// in progress but — unlike Flush — without trimming trailing whitespace.
// Used for progressive display so an inter-word space shows up as soon as
// it's fed rather than being trimmed away until more text follows it.
func (d *Decoder) Peek() string {
	d.flushChar()
	return d.output.String()
}

// Flush finalises any buffered character and returns the full decoded string.
func (d *Decoder) Flush() string {
	return strings.TrimSpace(d.Peek())
}

func (d *Decoder) flushChar() {
	code := d.current.String()
	d.current.Reset()
	if code == "" {
		return
	}
	d.output.WriteString(Decode(code))
}
