package dashboard

import (
	"bytes"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"mdm/internal/db"
)

// TestReleaseQAIsGone: the QA checklist, test cases, sign-off and "testing done" were
// removed from Releases on 21 Oct 2026. Their routes must not answer, and the release
// page's partials must render problems with no QA in them.
func TestReleaseQAIsGone(t *testing.T) {
	mux := http.NewServeMux()
	(&Handler{}).RegisterRoutes(mux)
	for _, path := range []string{
		"/releases/7/sign-off", "/releases/7/sign-off/clear", "/releases/7/test-results",
		"/test-cases", "/test-cases/3f2504e0-4f89-11d3-9a0c-0305e82c3301/edit",
		"/test-cases/3f2504e0-4f89-11d3-9a0c-0305e82c3301/delete",
	} {
		if _, pattern := mux.Handler(httptest.NewRequest("POST", path, nil)); pattern != "" {
			t.Errorf("POST %s still routes to %q", path, pattern)
		}
	}
	if _, pattern := mux.Handler(httptest.NewRequest("GET", "/releases/7/qa", nil)); pattern != "" {
		t.Errorf("GET /releases/7/qa still routes to %q", pattern)
	}

	src, err := os.ReadFile("../../templates/release_workspace.html")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`\{\{define "rd-qa`).Match(src) {
		t.Error("release_workspace.html still defines a QA partial")
	}

	// The htmx problems response: badges, counts and lists, nothing about QA.
	handlers, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(handlers), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["dict"] = func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	funcs["canRelease"] = func(string) bool { return true }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/release_workspace.html"))
	data := map[string]any{
		"Release":    &db.Release{ID: 7, Version: "v2.1.035"},
		"RelVersion": "v2.1.035", "Role": "admin", "CanReport": true, "CanAct": true,
		"Problems": []db.ReleaseProblem{
			{Title: "Kiosk freezes on boot", Severity: "blocker", Status: "open", Source: "qa", ReleaseID: 7},
			{Title: "Battery reads 0%", Severity: "major", Status: "fixed", Source: "manual", ReleaseID: 7, FixedInVersion: "v2.1.035"},
		},
		"ProblemSummary": db.ProblemSummary{Open: 2, Blockers: 1, Total: 2},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "release-oob", data); err != nil {
		t.Fatalf("release-oob: %v", err)
	}
	out := buf.String()
	for _, want := range []string{`id="rd-prob-list"`, `Kiosk freezes on boot`, `2 open · 1 blocker`, `value="verified"`} {
		if !strings.Contains(out, want) {
			t.Errorf("problems response is missing %q", want)
		}
	}
	for _, gone := range []string{"rd-qa", "from QA", "QA verification", "added to QA", "test case"} {
		if strings.Contains(out, gone) {
			t.Errorf("problems response still mentions %q", gone)
		}
	}
}
