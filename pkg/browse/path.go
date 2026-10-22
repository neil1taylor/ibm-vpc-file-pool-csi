package browse

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidatePath checks a path below the root. A name must not be empty, ".",
// "..", longer than MaxNameLen, or hold "/" or a NUL byte.
func ValidatePath(path [][]byte) error {
	if len(path) > MaxPathDepth {
		return &Error{Code: CodeBadPath, Msg: fmt.Sprintf("the path is deeper than %d folders", MaxPathDepth)}
	}
	for _, n := range path {
		if err := validateName(n); err != nil {
			return err
		}
	}
	return nil
}

func validateName(n []byte) error {
	switch {
	case len(n) == 0:
		return &Error{Code: CodeBadPath, Msg: "a name in the path is empty"}
	case len(n) > MaxNameLen:
		return &Error{Code: CodeBadPath, Msg: fmt.Sprintf("a name in the path is longer than %d bytes", MaxNameLen)}
	case bytes.Equal(n, []byte(".")) || bytes.Equal(n, []byte("..")):
		return &Error{Code: CodeBadPath, Msg: `"." and ".." are not allowed in a path`}
	case bytes.IndexByte(n, '/') >= 0:
		return &Error{Code: CodeBadPath, Msg: `a name in the path holds "/"`}
	case bytes.IndexByte(n, 0) >= 0:
		return &Error{Code: CodeBadPath, Msg: "a name in the path holds a NUL byte"}
	}
	return nil
}

// Escape shows a name as text: printable UTF-8 stays, a backslash is doubled
// and every other byte is written as \xNN. Unescape reverses it.
func Escape(name []byte) string {
	var b strings.Builder
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRune(name[i:])
		switch {
		case r == '\\' && size == 1:
			b.WriteString(`\\`)
		case r == utf8.RuneError && size <= 1, !unicode.IsPrint(r):
			for _, c := range name[i : i+size] {
				fmt.Fprintf(&b, `\x%02x`, c)
			}
		default:
			b.Write(name[i : i+size])
		}
		i += size
	}
	return b.String()
}

// Unescape reads text that Escape made.
func Unescape(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		if i+1 >= len(s) {
			return nil, fmt.Errorf("%q ends in a lone backslash", s)
		}
		switch s[i+1] {
		case '\\':
			out = append(out, '\\')
			i++
		case 'x':
			if i+3 >= len(s) {
				return nil, fmt.Errorf("%q has a short \\x escape", s)
			}
			v, ok := hexByte(s[i+2], s[i+3])
			if !ok {
				return nil, fmt.Errorf("%q has a bad \\x escape", s)
			}
			out = append(out, v)
			i += 3
		default:
			return nil, fmt.Errorf("%q has an unknown escape \\%c", s, s[i+1])
		}
	}
	return out, nil
}

func hexByte(a, b byte) (byte, bool) {
	hi, ok1 := hexVal(a)
	lo, ok2 := hexVal(b)
	return hi<<4 | lo, ok1 && ok2
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// SplitPath turns "/a/b" (names in escaped form) into a path. Empty parts are
// dropped. It does not validate the names.
func SplitPath(s string) ([][]byte, error) {
	var path [][]byte
	for _, part := range strings.Split(s, "/") {
		if part == "" {
			continue
		}
		n, err := Unescape(part)
		if err != nil {
			return nil, err
		}
		path = append(path, n)
	}
	return path, nil
}

// JoinPath shows a path as "/a/b" with escaped names.
func JoinPath(path [][]byte) string {
	if len(path) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, n := range path {
		b.WriteByte('/')
		b.WriteString(Escape(n))
	}
	return b.String()
}
