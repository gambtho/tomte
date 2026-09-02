package scaffold

import (
	"fmt"
	"strings"
)

// A hand-audited YAML emitter, deliberately not a library.
//
// This is #16's reasoning, kept: the generator's whole job is to turn
// operator-supplied text — a name, a description, a file of instructions —
// into a document that a cluster will then obey. The interesting failure is
// not a malformed file, it is a well-formed file that says something the
// operator did not write. Two rules make that impossible here:
//
//   - a block scalar indents EVERY line uniformly, so a line inside an
//     instruction file cannot dedent out of the scalar and become a sibling
//     key (a second `tools:`, say);
//   - a scalar that would have to span lines in flow style is REFUSED
//     rather than escaped, because escaping is where the subtle bugs live.
//
// (CWE-74, found in review on #16: an unquoted tool name containing a
// newline closed the sequence and appended a tool nobody had reviewed.)

// quote renders a scalar as a double-quoted YAML string. A value that
// contains a newline or any other control character is refused: nothing in
// an Agent's names, namespaces or tool lists legitimately spans lines, so a
// value that does is either a mistake or an attempt to inject one.
func quote(value string) (string, error) {
	for _, r := range value {
		if r == '\n' || r == '\r' || (r < 0x20 && r != '\t') || r == 0x7f {
			return "", fmt.Errorf("refusing to emit %q: a single-line YAML value may not contain control characters", value)
		}
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// blockScalar renders a literal block scalar (`|`) whose every line carries
// the same indent. Trailing whitespace is stripped per line so a line of
// spaces cannot change the block's indentation, and a line that is entirely
// empty is emitted empty rather than as indentation — both keep the block's
// shape independent of its content.
func blockScalar(value string, indent int) (string, error) {
	for _, r := range value {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return "", fmt.Errorf("refusing to emit a block scalar containing control characters")
		}
	}
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(value, "\n"), "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(pad)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String(), nil
}
