// Command upstream-watch compares the upstream versions this repo's ports track
// against the pins recorded in ext/versions.json, and prints a JSON report of any
// drift to stdout. It performs detection only — it never opens PRs, edits code, or
// bumps anything. The GitHub Action wrapper turns a drift finding into a GitHub
// issue (notify-only).
//
// Detection strategy per port:
//   - htmx:    the latest GitHub release tag of bigskysoftware/htmx
//     (GET /repos/{repo}/releases/latest -> .tag_name).
//   - preload: bigskysoftware/htmx-extensions publishes no per-extension release
//     tags, so we read the version field of src/preload/package.json on
//     the default branch (GET /repos/{repo}/contents/{path}/package.json,
//     base64 .content) and compare it to the pin.
//   - sigv4:   a Gothic-original port with no upstream JS package; emitted as an
//     informational note only, never as drift.
//
// Versions are compared after stripping a leading "v" and surrounding space, so
// "v2.1.0" and "2.1.0" are treated as equal.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// manifest mirrors ext/versions.json. Only the fields the watcher needs are
// modelled; unknown keys (like "$comment") are ignored.
type manifest struct {
	Htmx    manifestEntry `json:"htmx"`
	Preload manifestEntry `json:"preload"`
	Sigv4   manifestEntry `json:"sigv4"`
}

type manifestEntry struct {
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	Pinned   string `json:"pinned"`
	Upstream string `json:"upstream"`
	Note     string `json:"note"`
}

// finding is one line of the JSON report the workflow consumes.
type finding struct {
	Name   string `json:"name"`
	Repo   string `json:"repo,omitempty"`
	Pinned string `json:"pinned"`
	Latest string `json:"latest,omitempty"`
	Drift  bool   `json:"drift"`
	Title  string `json:"title,omitempty"` // issue title when Drift is true
	URL    string `json:"url,omitempty"`   // link to the upstream release/changelog
	Note   string `json:"note,omitempty"`
	Error  string `json:"error,omitempty"` // set when the upstream lookup failed
}

// report is the top-level stdout document.
type report struct {
	Findings []finding `json:"findings"`
	Drift    bool      `json:"drift"` // true if ANY finding drifted
}

func main() {
	var (
		manifestPath = flag.String("manifest", "ext/versions.json", "path to the versions manifest")
		apiBase      = flag.String("github-api", "https://api.github.com", "GitHub REST API base URL")
	)
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}
	rep, err := run(context.Background(), client, *manifestPath, *apiBase, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "upstream-watch:", err)
		os.Exit(1)
	}

	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "upstream-watch: marshal report:", err)
		os.Exit(1)
	}
	fmt.Println(string(out))

	// Surface the report to later workflow steps when running inside Actions.
	if gho := os.Getenv("GITHUB_OUTPUT"); gho != "" {
		if err := writeGitHubOutput(gho, rep, string(out)); err != nil {
			fmt.Fprintln(os.Stderr, "upstream-watch: write GITHUB_OUTPUT:", err)
		}
	}
}

// run loads the manifest and builds the report. It is the testable seam: tests
// point apiBase at an httptest server and assert on the returned report.
func run(ctx context.Context, client *http.Client, manifestPath, apiBase, token string) (report, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return report{}, fmt.Errorf("read manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return report{}, fmt.Errorf("parse manifest: %w", err)
	}

	var rep report
	rep.Findings = append(rep.Findings,
		checkHtmx(ctx, client, apiBase, token, m.Htmx),
		checkPreload(ctx, client, apiBase, token, m.Preload),
		checkSigv4(m.Sigv4),
	)
	for _, f := range rep.Findings {
		if f.Drift {
			rep.Drift = true
		}
	}
	return rep, nil
}

func checkHtmx(ctx context.Context, client *http.Client, apiBase, token string, e manifestEntry) finding {
	f := finding{Name: "htmx", Repo: e.Repo, Pinned: e.Pinned}
	tag, htmlURL, err := fetchLatestRelease(ctx, client, apiBase, token, e.Repo)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	f.Latest = normalizeVersion(tag)
	f.URL = htmlURL
	if driftBetween(e.Pinned, f.Latest) {
		f.Drift = true
		f.Title = fmt.Sprintf("Upstream htmx %s available (pinned %s)", f.Latest, normalizeVersion(e.Pinned))
	}
	return f
}

func checkPreload(ctx context.Context, client *http.Client, apiBase, token string, e manifestEntry) finding {
	f := finding{Name: "preload", Repo: e.Repo, Pinned: e.Pinned}
	ver, err := fetchPackageJSONVersion(ctx, client, apiBase, token, e.Repo, e.Path)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	f.Latest = normalizeVersion(ver)
	f.URL = fmt.Sprintf("https://github.com/%s/blob/HEAD/%s/package.json", e.Repo, strings.Trim(e.Path, "/"))
	if driftBetween(e.Pinned, f.Latest) {
		f.Drift = true
		f.Title = fmt.Sprintf("Upstream preload %s available (pinned %s)", f.Latest, normalizeVersion(e.Pinned))
	}
	return f
}

// checkSigv4 never flags drift: there is no upstream package to compare against.
func checkSigv4(e manifestEntry) finding {
	note := e.Note
	if note == "" {
		note = "Gothic-original port; no upstream package — manual review only."
	}
	return finding{Name: "sigv4", Pinned: e.Pinned, Drift: false, Note: note}
}

// driftBetween reports whether pinned and latest differ once normalized. A blank
// latest (lookup failed / not resolved) is never reported as drift.
func driftBetween(pinned, latest string) bool {
	l := normalizeVersion(latest)
	if l == "" {
		return false
	}
	return normalizeVersion(pinned) != l
}

func normalizeVersion(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	return strings.TrimSpace(s)
}

// fetchLatestRelease returns the tag_name and html_url of a repo's latest
// (non-prerelease) GitHub release.
func fetchLatestRelease(ctx context.Context, client *http.Client, apiBase, token, repo string) (tag, htmlURL string, err error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(apiBase, "/"), repo)
	body, err := getJSON(ctx, client, url, token)
	if err != nil {
		return "", "", err
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", "", fmt.Errorf("parse release: %w", err)
	}
	if rel.TagName == "" {
		return "", "", fmt.Errorf("release for %s has no tag_name", repo)
	}
	return rel.TagName, rel.HTMLURL, nil
}

// fetchPackageJSONVersion reads {path}/package.json from a repo's default branch
// via the contents API (base64-encoded content) and returns its "version".
func fetchPackageJSONVersion(ctx context.Context, client *http.Client, apiBase, token, repo, path string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/contents/%s/package.json",
		strings.TrimRight(apiBase, "/"), repo, strings.Trim(path, "/"))
	body, err := getJSON(ctx, client, url, token)
	if err != nil {
		return "", err
	}
	var content struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &content); err != nil {
		return "", fmt.Errorf("parse contents response: %w", err)
	}
	decoded := content.Content
	if content.Encoding == "base64" || strings.Contains(content.Content, "\n") {
		// The contents API returns base64 with embedded newlines.
		clean := strings.ReplaceAll(content.Content, "\n", "")
		b, derr := base64.StdEncoding.DecodeString(clean)
		if derr != nil {
			return "", fmt.Errorf("decode package.json content: %w", derr)
		}
		decoded = string(b)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(decoded), &pkg); err != nil {
		return "", fmt.Errorf("parse package.json: %w", err)
	}
	if pkg.Version == "" {
		return "", fmt.Errorf("package.json for %s/%s has no version", repo, path)
	}
	return pkg.Version, nil
}

func getJSON(ctx context.Context, client *http.Client, url, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gothic-upstream-watch")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func writeGitHubOutput(path string, rep report, jsonReport string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// Multi-line JSON needs the heredoc form of the Actions output file.
	fmt.Fprintf(f, "drift=%t\n", rep.Drift)
	fmt.Fprintf(f, "report<<UPSTREAM_WATCH_EOF\n%s\nUPSTREAM_WATCH_EOF\n", jsonReport)
	return nil
}
