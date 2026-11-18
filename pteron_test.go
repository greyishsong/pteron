package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	app    *app
	server *httptest.Server
	dir    string
}

func setup(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	app, err := newApp(dir, "100.64.0.2", 8848)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	return fixture{app, server, dir}
}
func (f fixture) request(t *testing.T, method, path string, payload any, status int, result any) []byte {
	t.Helper()
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, f.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, status, data)
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			t.Fatal(err)
		}
	}
	return data
}
func (f fixture) publish(t *testing.T, id, source string) Site {
	t.Helper()
	var site Site
	f.request(t, "POST", "/api/sites", map[string]string{"id": id, "html": source}, 201, &site)
	return site
}

func TestPublishingUpdatingListingAndDeletion(t *testing.T) {
	f := setup(t)
	var empty []Site
	f.request(t, "GET", "/api/sites", nil, 200, &empty)
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty listing should be []: %#v", empty)
	}
	original := "<!doctype html><html><head><title>Study &amp; plan</title></head><body><input name=notes></body></html>"
	first := f.publish(t, "", original)
	if !regexp.MustCompile(`^[a-z0-9]{8}$`).MatchString(first.ID) {
		t.Fatalf("bad generated ID: %s", first.ID)
	}
	if first.Title != "Study & plan" || first.URL != "http://100.64.0.2:8848/s/"+first.ID || first.LocalURL != "http://127.0.0.1:8848/s/"+first.ID {
		t.Fatalf("bad metadata: %+v", first)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.CreatedAt); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, "sites", first.ID, "index.html"))
	if err != nil || string(raw) != original {
		t.Fatalf("stored HTML was modified: %s, %v", raw, err)
	}
	second := f.publish(t, "custom-page_2", "<h1>Fallback <em>heading</em></h1>")
	if second.Title != "Fallback heading" {
		t.Fatalf("heading extraction: %q", second.Title)
	}
	endpoint := "/api/sites/" + first.ID
	f.request(t, "POST", endpoint+"/feedback", map[string]any{"fields": map[string]any{"notes": "keep this"}}, 200, nil)
	var updated Site
	f.request(t, "PUT", endpoint, map[string]string{"html": "<title>Revised</title><body>New</body>"}, 200, &updated)
	if updated.Title != "Revised" || updated.CreatedAt != first.CreatedAt || updated.UpdatedAt == first.UpdatedAt {
		t.Fatalf("bad update: %+v", updated)
	}
	if updated.Feedback.Fields["notes"] != "keep this" {
		t.Fatal("update lost feedback")
	}
	f.request(t, "PUT", endpoint, map[string]string{"html": "<p>No title</p>"}, 200, &updated)
	if updated.Title != "Revised" {
		t.Fatal("update without title must preserve title")
	}
	var sites []Site
	f.request(t, "GET", "/api/sites", nil, 200, &sites)
	if len(sites) != 2 || sites[0].ID != first.ID || sites[1].ID != second.ID {
		t.Fatalf("listing order: %+v", sites)
	}
	if sites[0].Feedback.Fields["notes"] != "keep this" {
		t.Fatal("list missing feedback")
	}
	f.request(t, "POST", "/api/sites", map[string]string{"id": first.ID, "html": "duplicate"}, 409, nil)
	f.request(t, "DELETE", endpoint, nil, 200, nil)
	f.request(t, "GET", "/s/"+first.ID, nil, 404, nil)
	f.request(t, "GET", endpoint+"/feedback", nil, 404, nil)
	f.request(t, "PUT", endpoint, map[string]string{"html": "cannot create"}, 404, nil)
	f.request(t, "DELETE", endpoint, nil, 404, nil)
	if _, err := os.Stat(filepath.Join(f.dir, "sites", first.ID)); !os.IsNotExist(err) {
		t.Fatalf("directory remains: %v", err)
	}
}

func TestHTMLInjectionAndAssets(t *testing.T) {
	f := setup(t)
	cases := []struct{ id, source, expected string }{
		{"body", "<html><BODY>Hello</BODY></html>", "<html><BODY>Hello%s</BODY></html>"},
		{"html", "<html>Hello</html>", "<html>Hello%s</html>"},
		{"fragment", "<h1>Hello</h1>", "<h1>Hello</h1>%s"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			f.publish(t, tc.id, tc.source)
			script := `<script src="/_pteron/runtime.js" data-site-id="` + tc.id + `"></script>`
			for _, suffix := range []string{"", "/"} {
				response, err := http.Get(f.server.URL + "/s/" + tc.id + suffix)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != 200 || string(data) != fmt.Sprintf(tc.expected, script) {
					t.Fatalf("injection: %s", data)
				}
				if response.Header.Get("Cache-Control") != "no-cache" {
					t.Fatal("HTML must not be cached")
				}
			}
		})
	}
	for path, contentType := range map[string]string{"/": "text/html", "/_pteron/runtime.js": "application/javascript"} {
		response, err := http.Get(f.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), contentType) {
			t.Fatalf("asset %s: %s", path, response.Status)
		}
	}
	f.request(t, "GET", "/s/body/unexpected", nil, 404, nil)
	f.request(t, "GET", "/unexpected", nil, 404, nil)
}

func TestFeedbackAutosaveSubmissionAndClear(t *testing.T) {
	f := setup(t)
	f.publish(t, "feedback", "<input name=notes>")
	path := "/api/sites/feedback/feedback"
	var feedback Feedback
	f.request(t, "GET", path, nil, 200, &feedback)
	if feedback.SiteID != "feedback" || feedback.Fields == nil || feedback.Extra == nil || feedback.Submitted {
		t.Fatalf("initial feedback: %+v", feedback)
	}
	fields := map[string]any{"option_a": true, "notes": "hello 世界", "count": 3, "choices": []string{"a", "b"}, "radio": nil}
	f.request(t, "POST", path, map[string]any{"fields": fields, "extra": map[string]any{"stage": 1}}, 200, &feedback)
	if feedback.Fields["notes"] != "hello 世界" || feedback.Fields["option_a"] != true || feedback.Fields["count"] != float64(3) || feedback.UpdatedAt == "" {
		t.Fatalf("autosave: %+v", feedback)
	}
	f.request(t, "PUT", path, map[string]any{"fields": map[string]any{"notes": "changed"}, "extra": map[string]any{"second": true}, "submitted": true}, 200, &feedback)
	submittedAt := feedback.SubmittedAt
	if !feedback.Submitted || submittedAt == "" || feedback.Fields["option_a"] != true || feedback.Extra["stage"] != float64(1) || feedback.Extra["second"] != true {
		t.Fatalf("merge/submit: %+v", feedback)
	}
	f.request(t, "POST", path, map[string]any{"fields": map[string]any{"notes": "after submit"}, "submitted": false}, 200, nil)
	f.request(t, "GET", path, nil, 200, &feedback)
	if !feedback.Submitted || feedback.SubmittedAt != submittedAt || feedback.Fields["notes"] != "after submit" {
		t.Fatalf("autosave reset submission: %+v", feedback)
	}
	// Reload from disk through a fresh app to verify persistence rather than memory.
	app, err := newApp(f.dir, "100.64.0.2", 8848)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
	var persisted Feedback
	if err := json.Unmarshal(recorder.Body.Bytes(), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Fields["notes"] != "after submit" {
		t.Fatal("feedback did not persist")
	}
	feedback = Feedback{}
	f.request(t, "DELETE", path, nil, 200, &feedback)
	if len(feedback.Fields) != 0 || len(feedback.Extra) != 0 || feedback.Submitted || feedback.SubmittedAt != "" {
		t.Fatalf("clear: %+v", feedback)
	}
}

func TestConcurrentFeedbackSaves(t *testing.T) {
	f := setup(t)
	f.publish(t, "parallel", "hello")
	var wg sync.WaitGroup
	failures := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := fmt.Sprintf(`{"fields":{"field_%d":%d}}`, i, i)
			response, err := http.Post(f.server.URL+"/api/sites/parallel/feedback", "application/json", strings.NewReader(payload))
			if err != nil {
				failures <- err
				return
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				failures <- fmt.Errorf("save status: %d", response.StatusCode)
			}
			var data Feedback
			if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
				failures <- err
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var feedback Feedback
	f.request(t, "GET", "/api/sites/parallel/feedback", nil, 200, &feedback)
	if len(feedback.Fields) != 32 {
		t.Fatalf("lost updates: %d fields", len(feedback.Fields))
	}
	data, err := os.ReadFile(filepath.Join(f.dir, "sites", "parallel", "feedback.json"))
	if err != nil || !json.Valid(data) {
		t.Fatalf("invalid on-disk JSON: %v", err)
	}
}

func TestSSEUpdateAndDelete(t *testing.T) {
	f := setup(t)
	f.publish(t, "live", "original")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", f.server.URL+"/api/sites/live/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("not SSE")
	}
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != ": connected\n" {
		t.Fatalf("initial SSE: %q, %v", line, err)
	}
	_, _ = reader.ReadString('\n')
	var updated Site
	f.request(t, "PUT", "/api/sites/live", map[string]string{"html": "updated"}, 200, &updated)
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: reload\n" {
		t.Fatalf("reload: %q, %v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.Contains(line, updated.UpdatedAt) {
		t.Fatalf("reload payload: %q, %v", line, err)
	}
	_, _ = reader.ReadString('\n')
	f.request(t, "DELETE", "/api/sites/live", nil, 200, nil)
	if line, err := reader.ReadString('\n'); err != io.EOF {
		t.Fatalf("deleted stream remains open: %q, %v", line, err)
	}
	f.request(t, "GET", "/api/sites/live/events", nil, 404, nil)
}

func TestValidationAndCrossOriginWrites(t *testing.T) {
	f := setup(t)
	for _, id := range []string{"../escape", "/tmp/escape", "foo/bar", "<script>", strings.Repeat("x", 65)} {
		f.request(t, "POST", "/api/sites", map[string]string{"id": id, "html": "x"}, 400, nil)
	}
	f.request(t, "POST", "/api/sites", map[string]string{"title": "missing HTML"}, 400, nil)
	req, _ := http.NewRequest("POST", f.server.URL+"/api/sites", strings.NewReader(`{"html":"x"}`))
	req.Header.Set("Origin", "https://unrelated.example")
	response, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(f.dir, "sites", "symlink")); err != nil {
		t.Fatal(err)
	}
	f.request(t, "PUT", "/api/sites/symlink", map[string]string{"html": "escape"}, 404, nil)
}

func TestCLIInterspersedFlags(t *testing.T) {
	t.Setenv("PTERON_PORT", "9000")
	t.Setenv("PTERON_DATA_DIR", "/tmp/pteron-test")
	opts, args, err := parseArgs([]string{"publish", "page.html", "--title", "A title", "--id=custom", "--json", "--port", "9999"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.port != 9999 || opts.dataDir != "/tmp/pteron-test" || opts.title != "A title" || opts.id != "custom" || !opts.json || strings.Join(args, " ") != "publish page.html" {
		t.Fatalf("CLI parsing: %+v, %v", opts, args)
	}
	_, args, err = parseArgs([]string{"--port=8848", "update", "custom", "-"})
	if err != nil || strings.Join(args, " ") != "update custom -" {
		t.Fatalf("stdin/global flags: %v, %v", args, err)
	}
	if _, _, err := parseArgs([]string{"serve", "--port", "0"}); err == nil {
		t.Fatal("invalid port accepted")
	}
}
