package providers

import "fmt"

// Factory builds a Provider from its config slice.
type Factory func(Config) (Provider, error)

// factories maps provider type → constructor. Providers register here via
// init(); the core never imports a concrete provider package directly except
// through blank imports collected in this file's package over time.
var factories = map[string]Factory{}

// Register adds a provider factory. Called from a provider package's init().
func Register(typ string, f Factory) {
	if _, dup := factories[typ]; dup {
		panic("providers: duplicate registration for type " + typ)
	}
	factories[typ] = f
}

// New constructs a provider for cfg.Type.
func New(cfg Config) (Provider, error) {
	f, ok := factories[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("providers: unknown provider type %q", cfg.Type)
	}
	return f(cfg)
}
