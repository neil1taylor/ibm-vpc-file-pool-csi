package util

import (
	"regexp"
	"strings"
)

// ShellQuote returns s as one word for /bin/sh: single quotes around it, with
// each single quote inside it closed, escaped and reopened.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var (
	rsyncOptionPattern = regexp.MustCompile(`^--?[A-Za-z][A-Za-z0-9-]*(=[A-Za-z0-9_.,:/@+*%-]+)?$`)
	basePathPattern    = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)
)

// ValidRsyncOption reports whether opt is a plain rsync flag, with an optional
// value of safe characters. Spaces, quotes and shell characters are refused.
func ValidRsyncOption(opt string) bool {
	return rsyncOptionPattern.MatchString(opt)
}

// ValidBasePath reports whether p is an absolute path of safe characters with
// no ".." element.
func ValidBasePath(p string) bool {
	if !basePathPattern.MatchString(p) {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
