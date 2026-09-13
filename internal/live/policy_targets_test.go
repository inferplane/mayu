package live

import (
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

func TestPolicyTargetsUseCurrentGeneration(t *testing.T) {
	h := &Holder{}
	if err := h.RoutedAndPriced("alias"); err == nil {
		t.Fatal("accepted absent topology")
	}
	// Bind before publishing: the callback must not capture a stale generation.
	validate := h.RoutedAndPriced
	models := map[string]config.ModelConfig{"m": {Aliases: []string{"alias"}, Targets: []config.Target{{Provider: "p", Model: "up"}, {Provider: "p", Model: "retry"}}}}
	provs := map[string]providers.Provider{"p": mockprovider.New("up")}
	rates := map[pricing.Key]pricing.Rate{{Provider: "p", Model: "up"}: {InputPerMTok: 1}}
	h.Swap(NewState(provs, models, pricing.New(pricing.OnMissingAllow, rates), nil))
	if err := validate("alias"); err == nil || !strings.Contains(err.Error(), "unpriced") {
		t.Fatalf("unpriced fallback accepted: %v", err)
	}
	rates[pricing.Key{Provider: "p", Model: "retry"}] = pricing.Rate{} // explicitly free is still a known rate
	h.Swap(NewState(provs, models, pricing.New(pricing.OnMissingAllow, rates), nil))
	if err := validate("alias"); err != nil {
		t.Fatalf("alias with all rates: %v", err)
	}
	if err := validate("missing"); err == nil {
		t.Fatal("accepted unrouted target")
	}
	h.Swap(NewState(nil, models, pricing.New(pricing.OnMissingAllow, rates), nil))
	if err := validate("alias"); err == nil || !strings.Contains(err.Error(), "unrouted") {
		t.Fatalf("missing provider accepted: %v", err)
	}
	h.Swap(NewState(provs, models, nil, nil))
	if err := validate("alias"); err == nil || !strings.Contains(err.Error(), "unpriced") {
		t.Fatalf("absent table accepted: %v", err)
	}
	h.Swap(NewState(provs, map[string]config.ModelConfig{"m": {}}, pricing.New(pricing.OnMissingAllow, rates), nil))
	if err := validate("m"); err == nil {
		t.Fatal("empty route accepted")
	}
}
