package api

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

// testPlaceholderIndex mirrors the committed internal/api/dist/index.html.
const testPlaceholderIndex = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>dl-tool</title></head>
<body><div id="root"></div></body></html>
`

func TestBaseHrefInjected(t *testing.T) {
	t.Run("web root", func(t *testing.T) {
		server := newTestServer(t, "")

		response := do(t, server.Router, http.MethodGet, "/")
		if response.Code != http.StatusOK {
			t.Fatalf("GET / = %d, want %d", response.Code, http.StatusOK)
		}

		body := response.Body.String()
		if !strings.Contains(body, `<base href="/">`) {
			t.Errorf("index.html at the web root misses <base href=\"/\">: %q", body)
		}
		if strings.Contains(body, `"/assets/`) {
			t.Errorf("index.html carries an absolute /assets/ URL: %q", body)
		}
	})

	t.Run("base path", func(t *testing.T) {
		server := newTestServer(t, "/dl-tool")

		response := do(t, server.Router, http.MethodGet, "/dl-tool/")
		if response.Code != http.StatusOK {
			t.Fatalf("GET /dl-tool/ = %d, want %d", response.Code, http.StatusOK)
		}

		body := response.Body.String()
		if !strings.Contains(body, `<base href="/dl-tool/">`) {
			t.Errorf("index.html under /dl-tool misses <base href=\"/dl-tool/\">: %q", body)
		}

		// The base element must sit ahead of every URL-bearing element,
		// directly after <head> (doc 10 section 7.3 rule 4).
		head := strings.Index(body, "<head>")
		base := strings.Index(body, "<base href=")
		if head < 0 || base != head+len("<head>") {
			t.Errorf("<base> is not the first child of <head>: head at %d, base at %d", head, base)
		}
	})
}

// TestNoInlineScript enforces doc 10 section 7.3 rule 4: the served
// index.html carries no <script> element without a src — the base href does
// the bootstrapping, and the CSP would block an inline script anyway.
func TestNoInlineScript(t *testing.T) {
	server := newTestServer(t, "/dl-tool")

	response := do(t, server.Router, http.MethodGet, "/dl-tool/")
	body := response.Body.String()

	for i, segment := range strings.Split(body, "<script") {
		if i == 0 {
			continue
		}

		end := strings.Index(segment, ">")
		if end < 0 {
			t.Fatalf("unterminated <script> tag in served index.html: %q", body)
		}
		if !strings.Contains(segment[:end], "src=") {
			t.Errorf("served index.html carries an inline <script> element: <script%s>", segment[:end])
		}
	}
}

func TestSPAFallbackInsideBase(t *testing.T) {
	server := newTestServer(t, "/dl-tool")

	response := do(t, server.Router, http.MethodGet, "/dl-tool/tasks/anything")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /dl-tool/tasks/anything = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), `<div id="root">`) {
		t.Errorf("fallback did not serve index.html: %q", response.Body.String())
	}
}

// TestSPAOutsideBaseIs404 complements TestOutsideBaseIs404 (server_test.go):
// with the SPA mounted, a path outside the base is still 404 — not a redirect
// and not the SPA — so a misconfigured proxy fails loudly.
func TestSPAOutsideBaseIs404(t *testing.T) {
	server := newTestServer(t, "/dl-tool")

	response := do(t, server.Router, http.MethodGet, "/tasks")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

func TestAPIRouteNotShadowed(t *testing.T) {
	server := newTestServer(t, "/dl-tool")

	response := do(t, server.Router, http.MethodGet, "/dl-tool/api/v1/system/info")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /dl-tool/api/v1/system/info = %d, want %d", response.Code, http.StatusOK)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	if !strings.Contains(response.Body.String(), `"version"`) {
		t.Errorf("system info body lost to the SPA fallback: %q", response.Body.String())
	}
}

// TestCacheHeaders asserts both rows of the task's caching table against an
// in-memory tree: the committed placeholder has no assets/ file, so the
// immutable row needs a content-hashed fixture.
func TestCacheHeaders(t *testing.T) {
	files := fstest.MapFS{
		indexPage:                {Data: []byte(testPlaceholderIndex)},
		"assets/app-4f3a2b1c.js": {Data: []byte("console.log('ok')")},
		"favicon.svg":            {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	}

	handler, err := newSPAHandler(files, "")
	if err != nil {
		t.Fatalf("newSPAHandler: %v", err)
	}

	for _, test := range []struct {
		name   string
		target string
		want   string
	}{
		{"index at root", "/", cacheNoCache},
		{"index by name", "/index.html", cacheNoCache},
		{"hashed asset", "/assets/app-4f3a2b1c.js", cacheImmutable},
		{"unhashed file", "/favicon.svg", cacheNoCache},
		{"unknown path falls back to index", "/tasks/anything", cacheNoCache},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := do(t, handler, http.MethodGet, test.target)
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want %d", test.target, response.Code, http.StatusOK)
			}
			if got := response.Header().Get("Cache-Control"); got != test.want {
				t.Errorf("GET %s Cache-Control = %q, want %q", test.target, got, test.want)
			}
		})
	}

	response := do(t, handler, http.MethodGet, "/assets/app-4f3a2b1c.js")
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") {
		t.Errorf("asset Content-Type = %q, want a javascript media type", contentType)
	}
}

func TestSPAMethodNotAllowed(t *testing.T) {
	server := newTestServer(t, "/dl-tool")

	response := do(t, server.Router, http.MethodPost, "/dl-tool/")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /dl-tool/ = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

// TestSPAHandlerRequiresHead guards the rewrite's one assumption: an
// index.html with no <head> element fails construction rather than serving
// an unrewritten page.
func TestSPAHandlerRequiresHead(t *testing.T) {
	files := fstest.MapFS{indexPage: {Data: []byte("<html><body/></html>")}}

	if _, err := newSPAHandler(files, ""); err == nil {
		t.Fatal("newSPAHandler accepted an index.html without <head>")
	}
}
