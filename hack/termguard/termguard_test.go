// Package termguard fails when the retired word "moment" appears in text a
// person reads. See docs/specs/2026-11-05-restore-point-term.md (step 6).
package termguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var momentWord = regexp.MustCompile(`(?i)moments?`)

// allowed names are stored data, names pods use to talk to each other, and
// deprecated aliases. Each is removed from a string before it is checked.
var allowed = []struct{ pattern, reason string }{
	{`\.moments\b`, "stored folder name; renaming would hide old backups"},
	{`\.repl-moment\b`, "stored file name in pool folders"},
	{`(?i)/v1/moments\b|/moments\b`, "browse path between pods of different versions"},
	{`(?i)\bX-Disk-Moment\b|\bRepl-Disk-Moment\b`, "headers between pods of different versions"},
	{`(?i)(?:json|yaml):"[^"]*moments?[^"]*"`, "stored or wire JSON key (saved file lists, helper-Job reports, gateway JSON, deprecated API fields)"},
	{`(?i)\\?"moments?(?:Truncated)?\\?"`, "quoted stored name: kopia tag, JSON key, or deprecated flag/field name"},
	{`--(?:list-moments|restore-moment|moments?)\b`, "deprecated flag names; hidden aliases kept for one release"},
	{`\$\{?moments?\b|\bmoments?=|\bmoment:|tag:moment`, "shell variable and kopia tag key inside helper-Job scripts (stored tag name, not shown)"},
	{`spec\.moment\w*`, "deprecated API field named in a deprecation notice"},
}

// allowedFiles hold deprecated names as their whole purpose.
var allowedFiles = map[string]string{
	"pkg/util/flagalias.go":        "maps deprecated flag names to new ones",
	"api/v1alpha1/restorepoint.go": "compat helpers that read and write the deprecated API fields",
}

// allowedLiterals are exact strings that are stored or wire names, not prose.
var allowedLiterals = map[string]string{
	"pkg/filelist/filelist.go": "moment",  // JSON key in saved file lists
	"pkg/gateway/gateway.go":   "moments", // gateway path name, unchanged on the wire
	"pkg/browse/server.go":     "moments", // label of the unchanged /v1/moments path
}

var allowedRE = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, a := range allowed {
		out = append(out, regexp.MustCompile(a.pattern))
	}
	return out
}()

func clean(s string) string {
	for _, re := range allowedRE {
		s = re.ReplaceAllString(s, " ")
	}
	return s
}

func repoRoot(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			t.Fatal("go.mod not found")
		}
		d = p
	}
}

// deprecatedDoc reports whether a comment group marks a deprecated API field.
func deprecatedDoc(cg *ast.CommentGroup) bool {
	return cg != nil && (strings.Contains(cg.Text(), "Deprecated") || strings.Contains(cg.Text(), "is the old name of"))
}

func TestNoMomentInUserVisibleText(t *testing.T) {
	root := repoRoot(t)
	var hits []string
	for _, dir := range []string{"cmd", "pkg", "api"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			if _, ok := allowedFiles[rel]; ok || strings.HasSuffix(rel, "zz_generated.deepcopy.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, p, nil, parser.ParseComments)
			if perr != nil {
				t.Errorf("parse %s: %v", rel, perr)
				return nil
			}
			report := func(pos token.Pos, kind, text string) {
				if momentWord.MatchString(clean(text)) {
					hits = append(hits, fset.Position(pos).String()[len(root)+1:]+": "+kind+": "+strings.TrimSpace(text))
				}
			}
			oldNames := map[*ast.BasicLit]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				// {Old: "moment", ...} in a util.FlagAlias is a deprecated flag name.
				if kv, ok := n.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Old" {
						if bl, ok := kv.Value.(*ast.BasicLit); ok {
							oldNames[bl] = true
						}
					}
				}
				return true
			})
			ast.Inspect(f, func(n ast.Node) bool {
				if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING && !oldNames[bl] {
					if s, err := strconv.Unquote(bl.Value); err == nil && allowedLiterals[rel] != s {
						report(bl.Pos(), "string", s)
					}
				}
				return true
			})
			if strings.HasPrefix(rel, "api/") {
				// Comments on API types and fields become CRD descriptions.
				skip := map[*ast.CommentGroup]bool{}
				ast.Inspect(f, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.Field:
						if deprecatedDoc(x.Doc) || deprecatedDoc(x.Comment) {
							skip[x.Doc], skip[x.Comment] = true, true
						}
					case *ast.GenDecl:
						if deprecatedDoc(x.Doc) {
							skip[x.Doc] = true
						}
					}
					return true
				})
				for _, cg := range f.Comments {
					if !skip[cg] {
						report(cg.Pos(), "comment", cg.Text())
					}
				}
			}
			return nil
		})
	}
	sort.Strings(hits)
	for _, h := range hits {
		t.Error(h)
	}
}

func TestAllowListIsShort(t *testing.T) {
	if len(allowed)+len(allowedFiles) > 25 {
		t.Fatalf("allow-list has %d entries; keep it short", len(allowed)+len(allowedFiles))
	}
}

// yamlAllowed are lines in charts/ and config/ that name a stored or
// deprecated field. Generated CRD files list deprecated fields by name.
var yamlAllowed = regexp.MustCompile(`(?i)^\s*(?:- )?\w*moments?\w*:\s*$|^\s*- \w*moments?\w*\s*$|\w*moments?\w* is the old name of|\.moments\b`)

func TestNoMomentInChartsAndConfig(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range []string{"charts", "config"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			for i, line := range strings.Split(string(b), "\n") {
				if momentWord.MatchString(line) && !yamlAllowed.MatchString(line) {
					t.Errorf("%s:%d: %s", rel, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
	}
}
