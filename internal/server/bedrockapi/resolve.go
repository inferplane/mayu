package bedrockapi

import (
	"sort"

	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/router"
)

func servesBedrockIngress(name string) bool {
	// mock is a test-only allowance, matching openaiapi.providerWire's
	// treatment of mock as an Anthropic-wire provider.
	return name == "bedrock" || name == "mock"
}

func resolveModel(r *router.Router, holder *live.Holder, urlID string) (string, bool) {
	canonical := r.Canonical(urlID)
	if _, _, err := r.ResolveChain(canonical); err == nil {
		return canonical, true
	}

	st := holder.Load()
	models := st.Models()
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		for _, target := range models[name].Targets {
			if target.Model != urlID {
				continue
			}
			prov, ok := st.Provider(target.Provider)
			if ok && servesBedrockIngress(prov.Name()) {
				return name, true
			}
		}
	}
	return "", false
}
