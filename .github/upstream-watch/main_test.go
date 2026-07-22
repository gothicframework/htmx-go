package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeGitHub serves the two REST endpoints the watcher hits, with configurable
// htmx release tag and preload package.json version.
func fakeGitHub(t *testing.T, htmxTag, preloadVersion string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/bigskysoftware/htmx/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tag_name": htmxTag,
			"html_url": "https://github.com/bigskysoftware/htmx/releases/tag/" + htmxTag,
		})
	})
	mux.HandleFunc("/repos/bigskysoftware/htmx-extensions/contents/src/preload/package.json", func(w http.ResponseWriter, r *http.Request) {
		pkg := fmt.Sprintf(`{"name":"htmx-ext-preload","version":%q}`, preloadVersion)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString([]byte(pkg)),
		})
	})
	return httptest.NewServer(mux)
}

// writeManifest drops a versions.json with the given pins into a temp dir.
func writeManifest(t *testing.T, htmxPinned, preloadPinned string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "versions.json")
	body := fmt.Sprintf(`{
	  "htmx":    {"repo":"bigskysoftware/htmx","pinned":%q},
	  "preload": {"repo":"bigskysoftware/htmx-extensions","path":"src/preload","pinned":%q},
	  "sigv4":   {"pinned":"n/a","note":"Gothic-original port; no upstream package."}
	}`, htmxPinned, preloadPinned)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func findingByName(t *testing.T, rep report, name string) finding {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no finding named %q", name)
	return finding{}
}

func newRun(t *testing.T, srv *httptest.Server, manifestPath string) report {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	rep, err := run(context.Background(), client, manifestPath, srv.URL, "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return rep
}

// Known-current: pins equal upstream -> no drift, no issue titles.
func TestNoDriftWhenCurrent(t *testing.T) {
	srv := fakeGitHub(t, "v2.0.9", "2.0.1")
	defer srv.Close()
	manifest := writeManifest(t, "2.0.9", "2.0.1")

	rep := newRun(t, srv, manifest)

	if rep.Drift {
		t.Fatalf("expected no drift, got Drift=true: %+v", rep.Findings)
	}
	htmx := findingByName(t, rep, "htmx")
	if htmx.Drift || htmx.Title != "" {
		t.Errorf("htmx: expected no drift, got %+v", htmx)
	}
	if htmx.Latest != "2.0.9" {
		t.Errorf("htmx: expected normalized latest 2.0.9, got %q", htmx.Latest)
	}
	preload := findingByName(t, rep, "preload")
	if preload.Drift || preload.Title != "" {
		t.Errorf("preload: expected no drift, got %+v", preload)
	}
}

// Known-stale: upstream ahead of pins -> drift on htmx and preload, with titles.
func TestDriftWhenStale(t *testing.T) {
	srv := fakeGitHub(t, "v2.1.0", "2.0.2")
	defer srv.Close()
	manifest := writeManifest(t, "2.0.9", "2.0.1")

	rep := newRun(t, srv, manifest)

	if !rep.Drift {
		t.Fatal("expected drift=true")
	}
	htmx := findingByName(t, rep, "htmx")
	if !htmx.Drift {
		t.Errorf("htmx: expected drift, got %+v", htmx)
	}
	if htmx.Latest != "2.1.0" {
		t.Errorf("htmx: expected latest 2.1.0, got %q", htmx.Latest)
	}
	wantTitle := "Upstream htmx 2.1.0 available (pinned 2.0.9)"
	if htmx.Title != wantTitle {
		t.Errorf("htmx: title = %q, want %q", htmx.Title, wantTitle)
	}
	if htmx.URL == "" {
		t.Error("htmx: expected a release URL")
	}
	preload := findingByName(t, rep, "preload")
	if !preload.Drift {
		t.Errorf("preload: expected drift, got %+v", preload)
	}
	if preload.Title != "Upstream preload 2.0.2 available (pinned 2.0.1)" {
		t.Errorf("preload: unexpected title %q", preload.Title)
	}
}

// sigv4 is informational: never drift, always a note, regardless of upstream.
func TestSigv4NeverDrifts(t *testing.T) {
	srv := fakeGitHub(t, "v9.9.9", "9.9.9")
	defer srv.Close()
	manifest := writeManifest(t, "2.0.9", "2.0.1")

	rep := newRun(t, srv, manifest)
	sig := findingByName(t, rep, "sigv4")
	if sig.Drift {
		t.Errorf("sigv4 must never drift, got %+v", sig)
	}
	if sig.Note == "" {
		t.Error("sigv4: expected an informational note")
	}
}

// A leading "v" on either side must not be read as drift.
func TestVPrefixNormalization(t *testing.T) {
	srv := fakeGitHub(t, "v2.0.9", "2.0.1")
	defer srv.Close()
	manifest := writeManifest(t, "v2.0.9", "v2.0.1")

	rep := newRun(t, srv, manifest)
	if rep.Drift {
		t.Errorf("v-prefixed equal versions must not drift: %+v", rep.Findings)
	}
}

func TestDriftBetween(t *testing.T) {
	cases := []struct {
		pinned, latest string
		want           bool
	}{
		{"2.0.9", "2.0.9", false},
		{"2.0.9", "v2.0.9", false},
		{"v2.0.9", "2.0.9", false},
		{"2.0.9", "2.1.0", true},
		{"2.0.9", "", false}, // unresolved latest is never drift
		{"2.0.1", "2.0.2", true},
	}
	for _, c := range cases {
		if got := driftBetween(c.pinned, c.latest); got != c.want {
			t.Errorf("driftBetween(%q,%q)=%v want %v", c.pinned, c.latest, got, c.want)
		}
	}
}
