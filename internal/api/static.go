package api

import (
	"bytes"
	"embed"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// The embed path is internal/api/dist because the Docker build stage copies
// the Vite output there (docs/10-deployment-and-compose.md section 5); the
// committed placeholder keeps go build working on a checkout with no web
// build. .gitignore keeps every other file of that tree out of the commit.
//
//go:embed all:dist
var distFS embed.FS

const (
	// indexPage is the SPA entry point and the fallback body for every
	// unknown path inside the base.
	indexPage = "index.html"
	// assetsPrefix holds Vite's content-hashed output — the only tree whose
	// bytes never change under a stable name, so it alone is immutable.
	assetsPrefix = "assets/"

	cacheNoCache   = "no-cache"
	cacheImmutable = "public, max-age=31536000, immutable"
)

// SPAHandler serves the embedded SPA. basePath is "" for the web root, or a
// path with a leading and no trailing slash. Unknown paths inside the base
// return index.html; the caller mounts this last so API routes win.
func SPAHandler(basePath string) (http.Handler, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded dist tree: %w", err)
	}

	handler, err := newSPAHandler(sub, basePath)
	if err != nil {
		return nil, err
	}

	return handler, nil
}

// spaHandler serves the built SPA from files, holding the base-rewritten
// index.html in memory so the embedded bytes are read once, at construction.
type spaHandler struct {
	files    fs.FS
	basePath string
	index    []byte
	served   time.Time
}

func newSPAHandler(files fs.FS, basePath string) (*spaHandler, error) {
	raw, err := fs.ReadFile(files, indexPage)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", indexPage, err)
	}

	index, err := injectBaseHref(raw, basePath)
	if err != nil {
		return nil, err
	}

	return &spaHandler{
		files:    files,
		basePath: basePath,
		index:    index,
		served:   time.Now(),
	}, nil
}

// injectBaseHref inserts <base href="{base}/"> immediately after the opening
// <head> tag, before every URL-bearing element (doc 10 section 7.3 rule 4).
// The bundled SPA reads document.baseURI; an inline bootstrap script is
// forbidden by the same rule and by the CSP. base is HTML-escaped before
// insertion; "" renders as the web root, <base href="/">.
func injectBaseHref(page []byte, basePath string) ([]byte, error) {
	head := findHeadTag(page)
	if head < 0 {
		return nil, fmt.Errorf("embedded %s carries no <head> element", indexPage)
	}

	closing := bytes.IndexByte(page[head:], '>')
	if closing < 0 {
		return nil, fmt.Errorf("embedded %s carries an unterminated <head> tag", indexPage)
	}

	at := head + closing + 1
	tag := []byte(`<base href="` + html.EscapeString(basePath) + `/">`)

	rewritten := make([]byte, 0, len(page)+len(tag))
	rewritten = append(rewritten, page[:at]...)
	rewritten = append(rewritten, tag...)
	rewritten = append(rewritten, page[at:]...)

	return rewritten, nil
}

// findHeadTag locates the opening <head> tag, skipping lookalikes such as
// <header>: the byte after "<head" must close or punctuate a tag.
func findHeadTag(page []byte) int {
	const tag = "<head"

	at := 0
	for {
		found := bytes.Index(page[at:], []byte(tag))
		if found < 0 {
			return -1
		}

		candidate := at + found
		if after := page[candidate+len(tag):]; len(after) > 0 && isTagBoundary(after[0]) {
			return candidate
		}
		at = candidate + len(tag)
	}
}

// isTagBoundary reports whether b can end a tag name.
func isTagBoundary(b byte) bool {
	switch b {
	case '>', ' ', '\t', '\n', '\r', '/':
		return true
	}

	return false
}

// ServeHTTP maps the request path — already confined to the base by the
// router — onto the embedded tree. A matching file is served with the cache
// policy of its tree; anything else answers the rewritten index.html so the
// SPA router resolves it. Only GET and HEAD are served.
func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeProblem(w, Problem(SlugNotFound, http.StatusMethodNotAllowed, "the SPA only serves GET and HEAD"))
		return
	}

	name := h.fileName(r.URL.Path)
	if name != "" && name != indexPage && h.serveNamed(w, r, name) {
		return
	}

	h.serveIndex(w, r)
}

// serveNamed serves one embedded file and reports whether it did so; a miss,
// a directory or a stat failure all fall through to the index fallback.
func (h *spaHandler) serveNamed(w http.ResponseWriter, r *http.Request, name string) bool {
	file, err := h.files.Open(name)
	if err != nil {
		return false
	}

	// Close cannot fail for an embedded or in-memory file; the error is still
	// logged rather than discarded, so no linter exception is needed.
	defer func() {
		if err := file.Close(); err != nil {
			logFromContext(r.Context()).Warn("close embedded asset", slog.String("asset", name), slog.Any("err", err))
		}
	}()

	stat, err := file.Stat()
	if err != nil || stat.IsDir() {
		return false
	}

	h.serveFile(w, r, file, name)

	return true
}

// fileName reduces the request path to a name inside the embedded tree: the
// base prefix goes, traversal collapses, and the root names index.html.
func (h *spaHandler) fileName(urlPath string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(urlPath, h.basePath))

	return strings.TrimPrefix(cleaned, "/")
}

// serveFile writes one embedded asset. The modtime is the handler's birth:
// embedded files carry no timestamp, and the process start is the closest
// honest "last changed" the binary has.
func (h *spaHandler) serveFile(w http.ResponseWriter, r *http.Request, file fs.File, name string) {
	w.Header().Set("Cache-Control", cachePolicy(name))
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}

	if seeker, ok := file.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, h.served, seeker)
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		writeProblem(w, Problem(SlugInternal, http.StatusInternalServerError, "an internal error occurred"))
		return
	}

	http.ServeContent(w, r, name, h.served, bytes.NewReader(data))
}

// serveIndex answers the rewritten index.html — the SPA fallback.
func (h *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", cacheNoCache)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, indexPage, h.served, bytes.NewReader(h.index))
}

// cachePolicy implements the task's caching table: content-hashed assets are
// immutable for a year; everything else revalidates, so a redeploy is picked
// up without a hard refresh.
func cachePolicy(name string) string {
	if strings.HasPrefix(name, assetsPrefix) {
		return cacheImmutable
	}

	return cacheNoCache
}
