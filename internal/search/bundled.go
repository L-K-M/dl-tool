// This file is the definition registry: the four bundled dlsearch/v1 engines
// plus every user definition under /config/engines. Bundled definitions are
// read-only; a user file whose id collides with one is rejected by name
// (docs/07-search-and-indexers.md sections 6 and 7).
package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/L-K-M/dl-tool/definitions"
	"github.com/L-K-M/dl-tool/internal/store"
)

// BundledIDs is the exact bundled set. FR-052 forbids a fifth entry.
var BundledIDs = []string{"academic-torrents", "arch-linux", "internet-archive", "linux-distributions"}

// bundledIDSet reserves the bundled ids even when a bundled document fails
// validation: a user file can never claim one.
var bundledIDSet = func() map[string]bool {
	m := make(map[string]bool, len(BundledIDs))
	for _, id := range BundledIDs {
		m[id] = true
	}
	return m
}()

// bundledSource is the Source value and the definition_source column value
// every bundled definition and its seeded indexer row carry.
const bundledSource = "bundled"

// bundledDir is the definitions.FS directory the embed pattern covers.
const bundledDir = "engines"

// userDefSuffix is the file name suffix user definitions must carry.
const userDefSuffix = ".dlsearch.yaml"

// seedProvenance is the provenance string every bundled indexer row records
// (docs/07-search-and-indexers.md section 7).
const seedProvenance = "shipped with dl-tool"

// Registry holds every loaded definition, bundled first, then user files.
// Bundled definitions are read-only; a user file whose id collides with one
// is rejected. The mutex guards the published maps because a later
// filesystem watcher calls Reload while request handlers call Get and List.
type Registry struct {
	log     *slog.Logger
	userDir string

	// bundled and bundledErrs are written once by NewRegistry and never
	// touched again, so Reload leaves the bundled half untouched.
	bundled     map[string]*Definition
	bundledErrs map[string]error

	mu      sync.RWMutex
	byID    map[string]*Definition
	sources map[string]string
	errs    map[string]error
}

// NewRegistry loads and validates definitions.FS, then userDir. A definition
// that fails validation is skipped with one warn record naming the file and
// the DefinitionError; it never blocks the others and never stops the process.
func NewRegistry(log *slog.Logger, userDir string) (*Registry, error) {
	r := &Registry{
		log:         log,
		userDir:     userDir,
		bundled:     map[string]*Definition{},
		bundledErrs: map[string]error{},
	}
	if err := r.loadBundled(); err != nil {
		return nil, err
	}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads and re-validates userDir. The bundled set is unaffected. It
// is the seam a later filesystem watcher calls; boot calls it once through
// NewRegistry.
func (r *Registry) Reload() error {
	byID, sources, errs, err := r.readUserDir()
	if err != nil {
		return err
	}
	r.publish(byID, sources, errs)
	return nil
}

// Get resolves one loaded definition by id.
func (r *Registry) Get(id string) (*Definition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	def, ok := r.byID[id]
	return def, ok
}

// List returns every loaded definition sorted by id.
func (r *Registry) List() []*Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Definition, 0, len(r.byID))
	for _, def := range r.byID {
		out = append(out, def)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Source reports where a definition came from: "bundled" or the absolute
// path of the user file that declared it. An unknown id returns "".
func (r *Registry) Source(id string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sources[id]
}

// Errors maps each rejected file to its *DefinitionError, surfaced in the UI.
// Bundled documents are keyed by their embedded path, user files by their
// absolute path.
func (r *Registry) Errors() map[string]error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]error, len(r.errs))
	for path, err := range r.errs {
		out[path] = err
	}
	return out
}

// SeedIndexers creates one indexers row per bundled definition that has none,
// with definition_source='bundled', legal_tier='legitimate', enabled=1,
// priority=50, provenance='shipped with dl-tool' and seeders_unknown taken
// from caps. It is idempotent: the unique index on definition_id makes a
// second boot a no-op.
func (r *Registry) SeedIndexers(ctx context.Context, s *store.IndexerStore) (created int, err error) {
	for _, id := range BundledIDs {
		def, ok := r.Get(id)
		if !ok {
			// A bundled definition that failed to load was already warned
			// about; there is no caps document to seed a row from.
			continue
		}
		row := store.Indexer{
			Name:             def.Name,
			Kind:             "dlsearch",
			Enabled:          true,
			DefinitionID:     &def.ID,
			DefinitionSource: ptr(bundledSource),
			Provenance:       ptr(seedProvenance),
			LegalTier:        "legitimate",
			Priority:         store.DefaultIndexerPriority,
			SeedersUnknown:   def.Caps.SeedersUnknown,
		}
		if _, err := s.Create(ctx, row, ""); err != nil {
			if errors.Is(err, store.ErrConflict) {
				// Already seeded on an earlier boot.
				continue
			}
			return created, fmt.Errorf("search: seed indexer %s: %w", id, err)
		}
		created++
	}
	return created, nil
}

// loadBundled reads every embedded engines/*.yaml through LoadDefinition. A
// bundled document that fails validation is a build defect: it is skipped
// with a warn record like any other failure, never silently trusted.
func (r *Registry) loadBundled() error {
	entries, err := fs.ReadDir(definitions.FS, bundledDir)
	if err != nil {
		return fmt.Errorf("search: read bundled engines: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := bundledDir + "/" + entry.Name()
		data, err := definitions.FS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("search: read bundled definition %s: %w", path, err)
		}
		def, err := LoadDefinition(data)
		if err != nil {
			r.bundledErrs[path] = err
			r.log.Warn("bundled definition rejected", "path", path, "err", err)
			continue
		}
		if _, dup := r.bundled[def.ID]; dup {
			dupErr := &DefinitionError{Msg: "duplicate bundled id: " + def.ID}
			r.bundledErrs[path] = dupErr
			r.log.Warn("bundled definition rejected", "path", path, "err", dupErr)
			continue
		}
		r.bundled[def.ID] = def
	}
	return nil
}

// readUserDir validates every *.dlsearch.yaml in userDir into three fresh
// maps; publish merges them over the bundled half. A definition that fails
// validation — or whose id collides with a bundled or earlier user
// definition — lands in errs and never reaches byID. A missing directory is
// an empty user set, not an error: /config/engines only exists when the
// operator bind-mounts it.
func (r *Registry) readUserDir() (byID map[string]*Definition, sources map[string]string, errs map[string]error, err error) {
	byID = map[string]*Definition{}
	sources = map[string]string{}
	errs = map[string]error{}

	entries, err := os.ReadDir(r.userDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return byID, sources, errs, nil
		}
		return nil, nil, nil, fmt.Errorf("search: read user engines dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), userDefSuffix) {
			continue
		}
		path, err := filepath.Abs(filepath.Join(r.userDir, entry.Name()))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("search: resolve %s: %w", entry.Name(), err)
		}
		data, err := r.readUserFile(path)
		if err != nil {
			errs[path] = err
			r.log.Warn("user definition rejected", "path", path, "err", err)
			continue
		}
		def, err := LoadDefinition(data)
		if err != nil {
			errs[path] = err
			r.log.Warn("user definition rejected", "path", path, "err", err)
			continue
		}
		if bundledIDSet[def.ID] || byID[def.ID] != nil {
			errs[path] = &DefinitionError{Msg: "id already in use: " + def.ID}
			r.log.Warn("user definition rejected", "path", path, "err", errs[path])
			continue
		}
		byID[def.ID] = def
		sources[def.ID] = path
	}
	return byID, sources, errs, nil
}

// readUserFile enforces the size cap while reading. entry.Info() would lstat —
// a symlink reports its own tiny size while os.ReadFile follows it to an
// arbitrarily large or endless target, and the file could grow between stat
// and read — so the check goes through the open handle: require a regular
// file, then bound the read itself.
func (r *Registry) readUserFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &DefinitionError{Msg: err.Error()}
	}
	// Close cannot fail meaningfully for a read-only file; the error is
	// logged rather than discarded, so no linter exception is needed.
	defer func() {
		if err := f.Close(); err != nil {
			r.log.Warn("close user definition", "path", path, "err", err)
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return nil, &DefinitionError{Msg: err.Error()}
	}
	if !info.Mode().IsRegular() {
		return nil, &DefinitionError{Msg: "not a regular file"}
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxDefinitionBytes+1))
	if err != nil {
		return nil, &DefinitionError{Msg: err.Error()}
	}
	if len(data) > MaxDefinitionBytes {
		// The read is capped at MaxDefinitionBytes+1, so report the larger
		// stat'd size — the real size of the dropped file.
		size := int64(len(data))
		if info.Size() > size {
			size = info.Size()
		}
		return nil, &DefinitionError{Msg: fmt.Sprintf(
			"document is %d bytes, over the %d-byte limit", size, MaxDefinitionBytes)}
	}
	return data, nil
}

// publish swaps the user half of the registry under the write lock, merged
// over the bundled half that never changes after NewRegistry.
func (r *Registry) publish(userByID map[string]*Definition, userSources map[string]string, userErrs map[string]error) {
	byID := make(map[string]*Definition, len(r.bundled)+len(userByID))
	sources := make(map[string]string, len(r.bundled)+len(userSources))
	errs := make(map[string]error, len(r.bundledErrs)+len(userErrs))
	for id, def := range r.bundled {
		byID[id] = def
		sources[id] = bundledSource
	}
	for path, err := range r.bundledErrs {
		errs[path] = err
	}
	for id, def := range userByID {
		byID[id] = def
		sources[id] = userSources[id]
	}
	for path, err := range userErrs {
		errs[path] = err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID = byID
	r.sources = sources
	r.errs = errs
}

func ptr[T any](v T) *T { return &v }
