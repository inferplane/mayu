package live

import "fmt"

// RoutedAndPriced validates an explicit policy target against the current
// topology. Aliases are allowed; model fallbacks cannot rescue an absent route.
// Every configured provider target must have a rate, even with on_missing=allow.
// Install this bound method once at startup; later policy applications observe
// the current generation after a topology reload.
func (h *Holder) RoutedAndPriced(model string) error {
	s := h.Load()
	if s == nil {
		return fmt.Errorf("live: unrouted model %q (no topology)", model)
	}
	canonical := s.Canonical(model)
	route, ok := s.Route(canonical)
	if !ok || len(route.Targets) == 0 {
		return fmt.Errorf("live: unrouted model %q", model)
	}
	for _, target := range route.Targets {
		if p, ok := s.Provider(target.Provider); !ok || p == nil {
			return fmt.Errorf("live: unrouted model %q (missing provider %q)", model, target.Provider)
		}
		if s.Pricing() == nil || !s.Pricing().HasRate(target.Provider, target.Model) {
			return fmt.Errorf("live: unpriced model %q target %q/%q", model, target.Provider, target.Model)
		}
	}
	return nil
}
