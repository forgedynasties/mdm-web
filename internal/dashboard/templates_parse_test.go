package dashboard

import (
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The dashboard parses its templates once, at startup, with template.Must — so a syntax
// error, or a call to a function that was never registered, panics the server before it
// listens. That failure reaches stage as a *green* deploy (the container starts, then
// dies), which is the most expensive way to find a typo.
//
// Parsing every template here with stub implementations of the real FuncMap's names
// catches both: ParseFiles resolves function identifiers at parse time, so a missing name
// fails exactly as it would in production, without needing the real implementations (many
// of which need config, a database or a request).
func TestTemplatesParse(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}

	// Helpers are registered two ways: inside the FuncMap literal as "name": <impl>, and
	// after it as funcMap["name"] = <impl> for the ones that need a closure over config.
	// Both must be found — a missed name fails the parse for a function that works fine
	// in production. Extra matches elsewhere in the file are harmless.
	pats := []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`),
		regexp.MustCompile(`funcMap\["([a-zA-Z][a-zA-Z0-9]*)"\]`),
	}
	funcs := template.FuncMap{}
	for _, re := range pats {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			funcs[m[1]] = func(...any) any { return "" }
		}
	}
	if len(funcs) < 40 {
		t.Fatalf("found only %d template function names — the extraction no longer matches "+
			"how funcMap is written, so this test would pass on a broken template", len(funcs))
	}

	files, err := filepath.Glob("../../templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found to parse: %v", err)
	}
	if _, err := template.New("").Funcs(funcs).ParseFiles(files...); err != nil {
		t.Fatalf("templates do not parse (the server would panic at startup): %v", err)
	}
}
