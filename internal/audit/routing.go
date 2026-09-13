package audit

import (
	"context"
	"slices"
)

// RoutingRef is a scalar-only projection of the routing decision. It must not
// contain a provider, topology, credentials, prompt, or detected value.
// This package deliberately has no dependency on router, config or server.
type RoutingRef struct {
	RequestedModel  string                  `json:"requested_model"`
	SelectedModel   string                  `json:"selected_model,omitempty"`
	ProposedModel   string                  `json:"proposed_model,omitempty"`
	Mode            string                  `json:"mode,omitempty"`
	Reason          string                  `json:"reason"`
	Inspection      string                  `json:"inspection"`
	Privacy         string                  `json:"privacy,omitempty"`
	Categories      []string                `json:"categories,omitempty"`
	Policies        []RoutingPolicyRef      `json:"policies,omitempty"`
	Recommendations []RoutingRecommendation `json:"recommendations,omitempty"`
	PlannedProvider string                  `json:"planned_provider,omitempty"`
	PlannedBoundary string                  `json:"planned_boundary,omitempty"`
	ActualModel     string                  `json:"actual_model,omitempty"`
	ActualProvider  string                  `json:"actual_provider,omitempty"`
	ActualBoundary  string                  `json:"actual_boundary,omitempty"`
	Masked          bool                    `json:"masked,omitempty"`
}

type RoutingPolicyRef struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
	Rule       string `json:"rule"`
}

type RoutingRecommendation struct {
	Policy RoutingPolicyRef `json:"policy"`
	Model  string           `json:"model"`
	Reason string           `json:"reason"`
}

type routingKey struct{}

func cloneRouting(r *RoutingRef) *RoutingRef {
	if r == nil {
		return nil
	}
	c := *r
	c.Categories = slices.Clone(r.Categories)
	c.Policies = slices.Clone(r.Policies)
	c.Recommendations = slices.Clone(r.Recommendations)
	return &c
}

// WithRouting takes a copy, so neither the caller nor an attempt can change the
// decision carried by started records waiting in the asynchronous writer.
func WithRouting(ctx context.Context, r *RoutingRef) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, routingKey{}, cloneRouting(r))
}

// RoutingFrom returns an owned projection for asynchronous record submission.
func RoutingFrom(ctx context.Context) *RoutingRef {
	r, _ := ctx.Value(routingKey{}).(*RoutingRef)
	return cloneRouting(r)
}

// WithRoutingAttempt derives a NEW context for one actual provider call. Never
// attach actual fields during routing, masking, context checks or admission.
func WithRoutingAttempt(ctx context.Context, model, provider, boundary string) context.Context {
	r := RoutingFrom(ctx)
	if r == nil {
		return ctx
	}
	if boundary == "" {
		boundary = "unknown"
	}
	r.ActualModel, r.ActualProvider, r.ActualBoundary = model, provider, boundary
	return context.WithValue(ctx, routingKey{}, r)
}
