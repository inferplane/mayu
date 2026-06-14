// Package filter is the core-side seam for request-transform plugins (the
// spec's filter chain ⑥). It holds ONLY the interface + a name registry — the
// concrete filters live under plugins/<name>/ and register themselves via a
// blank import in cmd/inferplane, exactly like providers. Core packages
// (server, router) import this interface, never a concrete plugin, so the
// plugin surface stays isolated (ADR-009).
package filter

import "sort"

// RequestFilter transforms request TEXT before it is forwarded upstream. A
// filter operates on extracted text only (never structural fields like
// cache_control / tool blocks / the system prompt); the caller is responsible
// for scoping which text spans are passed in. Mask returns the transformed text
// and the number of substitutions made (0 = unchanged).
type RequestFilter interface {
	Name() string
	Mask(text string) (masked string, redactions int)
}

// Masking is the resolved, per-request masking decision the assembly builds from
// the `plugins` config + the registry, and injects into the request handlers.
// It pairs the resolved filter with the team scope (Global = all teams). A nil
// *Masking, or one with a nil Filter, means masking is off — Enabled is
// false-safe so handlers can hold a single nil field.
type Masking struct {
	Filter RequestFilter
	Global bool
	Teams  map[string]bool
}

// Enabled reports whether the given team's requests must be masked.
func (m *Masking) Enabled(team string) bool {
	if m == nil || m.Filter == nil {
		return false
	}
	return m.Global || m.Teams[team]
}

var registry = map[string]RequestFilter{}

// Register adds a filter under its Name(). Called from a plugin's init(); a
// duplicate name panics (a programming error, surfaced at startup).
func Register(f RequestFilter) {
	name := f.Name()
	if _, dup := registry[name]; dup {
		panic("filter: duplicate registration for " + name)
	}
	registry[name] = f
}

// Get returns the registered filter for name (ok=false if absent). Config
// validation uses this to reject an unknown plugin name at load.
func Get(name string) (RequestFilter, bool) {
	f, ok := registry[name]
	return f, ok
}

// Names returns the registered filter names, sorted (for diagnostics/tests).
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
