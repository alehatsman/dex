package resolve

// stripJSONC removes // line comments, /* */ block comments, and trailing
// commas from a JSON-with-comments byte slice, leaving standard JSON that
// encoding/json can parse. tsconfig.json / jsconfig.json are JSONC in practice
// (TypeScript's parser tolerates comments and trailing commas), so we normalize
// before unmarshalling instead of pulling in a JSONC dependency.
//
// The scanner is string-aware: comment and comma handling is suppressed inside
// string literals, and escape sequences are honored so an escaped quote does
// not prematurely close a string.
func stripJSONC(in []byte) []byte {
	out := make([]byte, 0, len(in))

	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c == '"':
			// Copy the whole string literal verbatim, honoring escapes.
			out = append(out, c)
			i++
			for i < len(in) {
				out = append(out, in[i])
				if in[i] == '\\' && i+1 < len(in) {
					out = append(out, in[i+1])
					i += 2
					continue
				}
				if in[i] == '"' {
					break
				}
				i++
			}
		case c == '/' && i+1 < len(in) && in[i+1] == '/':
			// Line comment: skip to end of line.
			for i < len(in) && in[i] != '\n' {
				i++
			}
			// Preserve the newline for line-accurate downstream errors.
			if i < len(in) {
				out = append(out, in[i])
			}
		case c == '/' && i+1 < len(in) && in[i+1] == '*':
			// Block comment: skip to the closing */.
			i += 2
			for i+1 < len(in) && !(in[i] == '*' && in[i+1] == '/') {
				i++
			}
			i++ // land on '/', loop's i++ steps past it
		case c == ',':
			// Look ahead past whitespace/comments for a closing bracket; if the
			// next real token closes the container, this comma is trailing.
			if j := nextToken(in, i+1); j < len(in) && (in[j] == '}' || in[j] == ']') {
				continue // drop the trailing comma
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// nextToken returns the index of the next non-whitespace, non-comment byte at or
// after i, or len(in) if none remains.
func nextToken(in []byte, i int) int {
	for i < len(in) {
		switch {
		case in[i] == ' ' || in[i] == '\t' || in[i] == '\n' || in[i] == '\r':
			i++
		case in[i] == '/' && i+1 < len(in) && in[i+1] == '/':
			for i < len(in) && in[i] != '\n' {
				i++
			}
		case in[i] == '/' && i+1 < len(in) && in[i+1] == '*':
			i += 2
			for i+1 < len(in) && !(in[i] == '*' && in[i+1] == '/') {
				i++
			}
			i += 2
		default:
			return i
		}
	}
	return i
}
