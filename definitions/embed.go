// Package definitions carries the four bundled dlsearch/v1 engine documents
// compiled into the binary (docs/07-search-and-indexers.md section 6). The
// embed lives at the repository root because an internal/ package cannot
// embed a parent directory.
package definitions

import "embed"

// FS holds engines/*.yaml, the complete bundled set of FR-052. The documents
// are read-only at runtime; user definitions load from /config/engines.
//
//go:embed engines/*.yaml
var FS embed.FS
