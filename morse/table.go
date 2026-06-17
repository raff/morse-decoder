package morse

// table maps ITU-R Morse sequences to Unicode runes.
var table = map[string]rune{
	// Letters
	".-":   'A',
	"-...": 'B',
	"-.-.": 'C',
	"-..":  'D',
	".":    'E',
	"..-.": 'F',
	"--.":  'G',
	"....": 'H',
	"..":   'I',
	".---": 'J',
	"-.-":  'K',
	".-..": 'L',
	"--":   'M',
	"-.":   'N',
	"---":  'O',
	".--.": 'P',
	"--.-": 'Q',
	".-.":  'R',
	"...":  'S',
	"-":    'T',
	"..-":  'U',
	"...-": 'V',
	".--":  'W',
	"-..-": 'X',
	"-.--": 'Y',
	"--..": 'Z',

	// Digits
	"-----": '0',
	".----": '1',
	"..---": '2',
	"...--": '3',
	"....-": '4',
	".....": '5',
	"-....": '6',
	"--...": '7',
	"---..": '8',
	"----.": '9',

	// Punctuation
	".-.-.-":  '.',
	"--..--":  ',',
	"..--..":  '?',
	".----.":  '\'',
	"-.-.--":  '!',
	"-..-.":   '/',
	"-.--.":   '(',
	"-.--.-":  ')',
	".-...":   '&',
	"---...":  ':',
	"-.-.-.":  ';',
	"-...-":   '=',
	".-.-.":   '+',
	"-....-":  '-',
	"..--.-":  '_',
	".-..-.":  '"',
	"...-..-": '$',
	".--.-.":  '@',
}

// prosigns maps Morse sequences sent as a single run (no inter-character gap)
// to their procedural-signal rendering. A prosign is just two or more letters
// keyed together; the decoder only sees them as one code because no character
// gap separates the elements.
//
// Codes that collide with a punctuation mark already in `table` are rendered in
// a combined form like "<AS/&>" so neither meaning is hidden — they are
// genuinely the same dots and dashes and cannot be told apart by timing.
var prosigns = map[string]string{
	// Collisions with punctuation (shown as <prosign/punctuation>).
	".-...": "<AS/&>", // wait / ampersand
	".-.-.": "<AR/+>", // end of message / plus
	"-...-": "<BT/=>", // break (new paragraph) / equals
	"-.--.": "<KN/(>", // go ahead, named station only / open paren

	// Unambiguous prosigns (no punctuation assigned to the same code).
	"...-.-":    "<SK>",  // end of contact (a.k.a. VA)
	"-...-.-":   "<BK>",  // break-in
	"-.-.-":     "<CT>",  // start of transmission (a.k.a. KA)
	".-.-":      "<AA>",  // new line
	"...-.":     "<SN>",  // understood (a.k.a. VE)
	"..-.-":     "<INT>", // interrogative / repeat
	"...---...": "<SOS>", // distress
	"........":  "<HH>",  // error / correction
	"-.-..-..":  "<CL>",  // closing down
}

// Decode returns the text for a Morse sequence: a prosign rendering if the code
// is a known procedural signal, otherwise the single character from `table`,
// or "?" if unknown. Prosigns take precedence over punctuation for colliding
// codes (the combined "<AS/&>" form keeps both meanings visible).
func Decode(code string) string {
	if p, ok := prosigns[code]; ok {
		return p
	}
	if ch, ok := table[code]; ok {
		return string(ch)
	}
	return "?"
}

// Lookup returns the character for a Morse code sequence, or '?' if unknown.
// It only resolves single-character codes; use Decode for prosign-aware output.
func Lookup(code string) rune {
	if ch, ok := table[code]; ok {
		return ch
	}
	return '?'
}
