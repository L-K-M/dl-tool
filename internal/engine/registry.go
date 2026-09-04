package engine

import (
	"sort"
	"sync"
)

// Registry holds one Engine per Name(). Instances are injected at the
// composition root; the registry itself never imports a concrete adapter
// (docs/06-download-engines.md §1). Adding an engine to dl-tool is one
// Register call beside the adapter's constructor.
type Registry struct {
	mu      sync.Mutex
	engines map[string]Engine
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{engines: make(map[string]Engine)}
}

// Register adds e under e.Name(). A duplicate name is a composition bug —
// two engines claiming one name — so it panics rather than silently
// shadowing the first registration.
func (r *Registry) Register(e Engine) {
	// Name() is an arbitrary interface call; take it before the lock so an
	// implementation that re-enters the registry cannot deadlock.
	name := e.Name()

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, dup := r.engines[name]; dup {
		panic("engine: duplicate registration: " + name)
	}
	r.engines[name] = e
}

// Get returns the engine registered under name, and whether one was.
func (r *Registry) Get(name string) (Engine, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.engines[name]
	return e, ok
}

// Names returns every registered engine name, sorted for stable iteration.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := make([]string, 0, len(r.engines))
	for name := range r.engines {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
