// Package bedrockapi implements the AWS Bedrock InvokeModel ingress.
package bedrockapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/bodystore"
	"github.com/inferplane/inferplane/internal/filter"
	"github.com/inferplane/inferplane/internal/governance"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/live"
	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/tracing"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/pkg/ulid"
	"github.com/inferplane/inferplane/providers"
	"go.opentelemetry.io/otel/trace"
)

const (
	ingressName        = "bedrock"
	rejectedModelLabel = "_rejected"
)

type InvokeHandler struct {
	r          *router.Router
	holder     *live.Holder
	aud        *audit.Writer
	gov        *governance.Governor
	metrics    *metrics.Metrics
	mask       *filter.Masking
	teamPolicy func(team string) (keystore.TeamRecord, bool)
	bodies     *bodystore.Recorder
	streaming  bool
}

func NewInvokeHandler(r *router.Router, holder *live.Holder, streaming bool) *InvokeHandler {
	return &InvokeHandler{r: r, holder: holder, streaming: streaming}
}

func NewInvokeHandlerMetrics(r *router.Router, holder *live.Holder, aud *audit.Writer, gov *governance.Governor, m *metrics.Metrics, streaming bool) *InvokeHandler {
	return &InvokeHandler{
		r: r, holder: holder, aud: aud, gov: gov, metrics: m, streaming: streaming,
	}
}

func (h *InvokeHandler) SetMasking(m *filter.Masking) { h.mask = m }

func (h *InvokeHandler) SetTeamPolicy(fn func(team string) (keystore.TeamRecord, bool)) {
	h.teamPolicy = fn
}

func (h *InvokeHandler) SetBodyRecorder(r *bodystore.Recorder) { h.bodies = r }

func (h *InvokeHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return
	}

	// Parsed is observation-only. Bedrock requests are always forwarded from
	// RawBody so unknown fields and prompt-cache bytes remain untouched.
	var parsed schema.ChatRequest
	_ = json.Unmarshal(raw, &parsed)

	urlID := req.PathValue("modelId")
	model, resolved := resolveModel(h.r, h.holder, urlID)

	tctx := tracing.Extract(req.Context(), req.Header)
	tctx, span := tracing.Start(tctx, "invoke "+model)
	defer span.End()
	req = req.WithContext(tctx)
	tracing.SetGenAIRequest(span, model)
	traceID := tracing.TraceID(tctx)

	p, ok := principal.From(req.Context())
	if !ok {
		tracing.SetStatus(span, false, "no principal")
		writeErr(w, http.StatusUnauthorized, "no principal")
		return
	}
	if !resolved {
		h.audit(p, urlID, "", &audit.OutcomeRef{Status: http.StatusNotFound}, false, traceID)
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, http.StatusNotFound, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "unknown model")
		writeErr(w, http.StatusNotFound, "model not found")
		return
	}
	if !h.r.Allows(p, model) {
		h.audit(p, model, "", &audit.OutcomeRef{Status: http.StatusForbidden, Error: audit.DenyModelNotAllowed.Ptr()}, false, traceID)
		h.metrics.ObserveRequest(ingressName, model, "", p.Team, http.StatusForbidden, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "model not allowed")
		writeErr(w, http.StatusForbidden, "model not allowed for this key")
		return
	}

	chain, st, err := h.r.ResolveChain(model)
	if err != nil {
		h.audit(p, model, "", &audit.OutcomeRef{Status: http.StatusNotFound}, false, traceID)
		h.metrics.ObserveRequest(ingressName, rejectedModelLabel, "", p.Team, http.StatusNotFound, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "unknown model")
		writeErr(w, http.StatusNotFound, "model not found")
		return
	}
	filtered := make([]router.ChainTarget, 0, len(chain))
	for _, ct := range chain {
		if servesBedrockIngress(ct.Provider.Name()) {
			filtered = append(filtered, ct)
		}
	}
	if len(filtered) == 0 {
		h.audit(p, model, "", &audit.OutcomeRef{Status: http.StatusNotFound}, false, traceID)
		h.metrics.ObserveRequest(ingressName, model, "", p.Team, http.StatusNotFound, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "no bedrock target")
		writeErr(w, http.StatusNotFound, "model not found")
		return
	}
	chain = filtered

	var teamRec keystore.TeamRecord
	if h.teamPolicy != nil {
		if rec, ok := h.teamPolicy(p.Team); ok {
			teamRec = rec
		}
	}
	if len(teamRec.AllowedRegions) > 0 {
		if filtered := router.FilterRegions(chain, teamRec.AllowedRegions); len(filtered) == 0 {
			h.audit(p, model, "", &audit.OutcomeRef{Status: http.StatusForbidden, Error: audit.DenyRegionBlocked.Ptr()}, false, traceID)
			h.metrics.ObserveRequest(ingressName, model, "", p.Team, http.StatusForbidden, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "region blocked")
			writeErr(w, http.StatusForbidden, "no allowed-region target for model")
			return
		} else {
			chain = filtered
		}
	}

	piiMasked := false
	if h.mask.Enabled(p.Team) {
		masked, n, err := maskBody(raw, h.mask.Filter)
		if err != nil {
			h.audit(p, model, chain[0].Upstream, &audit.OutcomeRef{Status: http.StatusBadRequest}, false, traceID)
			h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, http.StatusBadRequest, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "pii mask failed")
			writeErr(w, http.StatusBadRequest, "request could not be PII-masked")
			return
		}
		if n > 0 {
			var reparsed schema.ChatRequest
			if err := json.Unmarshal(masked, &reparsed); err != nil {
				h.audit(p, model, chain[0].Upstream, &audit.OutcomeRef{Status: http.StatusBadRequest}, false, traceID)
				h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, http.StatusBadRequest, time.Since(start).Seconds(), 0)
				tracing.SetStatus(span, false, "pii mask failed")
				writeErr(w, http.StatusBadRequest, "request could not be PII-masked")
				return
			}
			raw = masked
			parsed = reparsed
			piiMasked = true
			h.metrics.ObservePIIMask(p.Team, n)
		}
	}

	table := st.Pricing()
	if h.gov != nil {
		dec := h.gov.PreCheck(p.Team, p.KeyID, keyPolicyOf(p), estimateTokens(raw))
		if !dec.Allowed {
			h.audit(p, model, chain[0].Upstream, &audit.OutcomeRef{Status: dec.Status, Error: dec.Code.Ptr()}, piiMasked, traceID)
			h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, dec.Status, time.Since(start).Seconds(), 0)
			tracing.SetStatus(span, false, "governance deny")
			writeErr(w, dec.Status, dec.Reason)
			return
		}
	}

	h.audit(p, model, chain[0].Upstream, nil, piiMasked, traceID)
	if h.streaming {
		writeErr(w, http.StatusInternalServerError, "streaming not implemented yet")
		h.auditCompleted(ulid.New(), p, model, chain[0].Upstream, http.StatusInternalServerError, nil, nil, traceID, "", teamRec.GuardrailID, teamRec.GuardrailVersion)
		h.metrics.ObserveRequest(ingressName, model, chain[0].ProviderName, p.Team, http.StatusInternalServerError, time.Since(start).Seconds(), 0)
		tracing.SetStatus(span, false, "streaming not implemented")
		return
	}

	for i, ct := range chain {
		upHeaders := req.Header.Clone()
		tracing.Inject(req.Context(), upHeaders)
		pr := &providers.ProxyRequest{
			Model: model, Upstream: ct.Upstream, Parsed: &parsed,
			RawBody: raw, Headers: upHeaders, Stream: h.streaming,
			IngressProtocol:  "bedrock",
			GuardrailID:      teamRec.GuardrailID,
			GuardrailVersion: teamRec.GuardrailVersion,
		}
		last := i == len(chain)-1
		if i > 0 {
			w.Header().Set("x-inferplane-fallback", ct.ProviderName)
		}
		if !h.serveComplete(w, req, ct.Provider, pr, p, model, ct.ProviderName, ct.Identity, ct.Upstream, last, start, table) {
			return
		}
		h.r.RecordResult(ct.ProviderName, ct.Identity, false)
		h.metrics.ObserveFallback(model, ct.ProviderName, chain[i+1].ProviderName, "upstream_error")
	}
}

func (h *InvokeHandler) serveComplete(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest, p keystore.Principal, model, providerName, identity, upstream string, last bool, start time.Time, table *pricing.Table) (retriable bool) {
	resp, err := prov.Complete(req.Context(), pr)
	if err != nil {
		if !last {
			return true
		}
		var ue *providers.UpstreamError
		if errors.As(err, &ue) {
			writeErr(w, ue.StatusCode, "bedrock upstream error")
			h.auditCompleted(ulid.New(), p, model, upstream, ue.StatusCode, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
			recordSpanResponse(req, prov.Name(), upstream, nil, false)
			h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, ue.StatusCode, time.Since(start).Seconds(), 0)
			return false
		}
		writeErr(w, http.StatusBadGateway, "bedrock upstream error")
		h.auditCompleted(ulid.New(), p, model, upstream, http.StatusBadGateway, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
		recordSpanResponse(req, prov.Name(), upstream, nil, false)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, http.StatusBadGateway, time.Since(start).Seconds(), 0)
		return false
	}
	if resp == nil {
		if !last {
			return true
		}
		writeErr(w, http.StatusBadGateway, "bedrock upstream error")
		h.auditCompleted(ulid.New(), p, model, upstream, http.StatusBadGateway, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
		recordSpanResponse(req, prov.Name(), upstream, nil, false)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, http.StatusBadGateway, time.Since(start).Seconds(), 0)
		return false
	}
	if !last && (resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests) {
		return true
	}
	if resp.Headers != nil {
		copyUpstreamHeaders(w.Header(), resp.Headers)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		writeErr(w, resp.StatusCode, "bedrock upstream error")
		h.auditCompleted(ulid.New(), p, model, upstream, resp.StatusCode, nil, nil, tracing.TraceID(req.Context()), "", pr.GuardrailID, pr.GuardrailVersion)
		recordSpanResponse(req, prov.Name(), upstream, nil, false)
		h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, resp.StatusCode, time.Since(start).Seconds(), 0)
		return false
	}

	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	var usage *audit.UsageRef
	var cost *audit.CostRef
	if resp.Parsed != nil {
		if u := resp.Parsed.Usage; u != nil {
			if u.InputTokens != nil {
				w.Header().Set("X-Amzn-Bedrock-Input-Token-Count", strconv.FormatInt(*u.InputTokens, 10))
			}
			if u.OutputTokens != nil {
				w.Header().Set("X-Amzn-Bedrock-Output-Token-Count", strconv.FormatInt(*u.OutputTokens, 10))
			}
		}
		usage = usageRef(resp.Parsed.Usage)
		cost = h.settle(p, providerName, model, upstream, resp.Parsed.Usage, table)
		h.observeTokens(model, providerName, p.Team, resp.Parsed.Usage)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.RawBody)
	h.r.RecordResult(providerName, identity, true)

	recID := ulid.New()
	var bodyRef string
	if h.bodies != nil {
		bodyRef = h.bodies.Capture(recID, p.Team, pr.RawBody, resp.RawBody)
	}
	h.auditCompleted(recID, p, model, upstream, resp.StatusCode, usage, cost, tracing.TraceID(req.Context()), bodyRef, pr.GuardrailID, pr.GuardrailVersion)
	recordSpanResponse(req, prov.Name(), upstream, usage, true)
	h.metrics.ObserveRequest(ingressName, model, providerName, p.Team, resp.StatusCode, time.Since(start).Seconds(), 0)
	return false
}

func (h *InvokeHandler) settle(p keystore.Principal, providerName, model, upstream string, u *schema.Usage, table *pricing.Table) *audit.CostRef {
	if h.gov == nil || u == nil {
		return nil
	}
	pu := pricing.Usage{
		Input:        deref(u.InputTokens),
		Output:       deref(u.OutputTokens),
		CacheRead:    deref(u.CacheReadInputTokens),
		CacheWrite5m: deref(u.CacheCreationInputTokens),
	}
	cost, missing := h.gov.Settle(p.Team, p.KeyID, keyPolicyOf(p), providerName, upstream, pu, table)
	return &audit.CostRef{
		AmountUSDMicros: cost,
		PricingMissing:  missing,
		PricingVersion:  governance.PricingVersionOf(table),
	}
}

func keyPolicyOf(p keystore.Principal) governance.KeyPolicy {
	return governance.KeyPolicy{
		RatePerMin: p.RPM, TokensPerMinute: p.TPM, BudgetMicrosPerMonth: p.BudgetUSDMicros,
	}
}

func estimateTokens(raw []byte) int64 {
	n := int64(len(raw) / 4)
	if n < 1 {
		n = 1
	}
	return n
}

func (h *InvokeHandler) observeTokens(model, provider, team string, u *schema.Usage) {
	if u == nil {
		return
	}
	h.metrics.ObserveTokenUsage("input", model, provider, team, deref(u.InputTokens))
	h.metrics.ObserveTokenUsage("output", model, provider, team, deref(u.OutputTokens))
	h.metrics.ObserveTokenUsage("cache_read", model, provider, team, deref(u.CacheReadInputTokens))
	h.metrics.ObserveTokenUsage("cache_write_5m", model, provider, team, deref(u.CacheCreationInputTokens))
}

func copyUpstreamHeaders(dst http.Header, src http.Header) {
	for k, values := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Transfer-Encoding", "Connection":
			continue
		}
		for _, value := range values {
			dst.Add(k, value)
		}
	}
}

func (h *InvokeHandler) audit(p keystore.Principal, model, upstream string, outcome *audit.OutcomeRef, piiMasked bool, traceID string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_started",
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request: audit.RequestRef{
			Ingress: "bedrock", ModelRequested: model, ModelResolved: upstream,
			Stream: h.streaming, PIIMasked: piiMasked,
		},
		Outcome: outcome,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	h.aud.Append(rec)
}

func (h *InvokeHandler) auditCompleted(id string, p keystore.Principal, model, upstream string, status int, usage *audit.UsageRef, cost *audit.CostRef, traceID, bodyRef, guardrailID, guardrailVersion string) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1,
		Event:         "request_completed",
		ID:            id,
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request: audit.RequestRef{
			Ingress: "bedrock", ModelRequested: model, ModelResolved: upstream, Stream: h.streaming,
		},
		Outcome: &audit.OutcomeRef{Status: status},
		Usage:   usage,
		Cost:    cost,
	}
	if traceID != "" {
		rec.TraceID = &traceID
	}
	if bodyRef != "" {
		rec.BodyRef = &bodyRef
	}
	if guardrailID != "" {
		rec.GuardrailID = &guardrailID
	}
	if guardrailVersion != "" {
		rec.GuardrailVersion = &guardrailVersion
	}
	h.aud.Append(rec)
}

func recordSpanResponse(req *http.Request, system, upstream string, usage *audit.UsageRef, ok bool) {
	span := trace.SpanFromContext(req.Context())
	var input, output int64
	if usage != nil {
		input, output = usage.InputTokens, usage.OutputTokens
	}
	tracing.SetGenAIResponse(span, system, upstream, input, output)
	tracing.SetStatus(span, ok, "")
}

func usageRef(u *schema.Usage) *audit.UsageRef {
	if u == nil {
		return nil
	}
	return &audit.UsageRef{
		InputTokens:              deref(u.InputTokens),
		OutputTokens:             deref(u.OutputTokens),
		CacheReadInputTokens:     deref(u.CacheReadInputTokens),
		CacheCreationInputTokens: deref(u.CacheCreationInputTokens),
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func maskBody(raw []byte, f filter.RequestFilter) ([]byte, int, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, 0, fmt.Errorf("maskBody: %w", err)
	}
	messagesRaw, ok := top["messages"]
	if !ok {
		return raw, 0, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		return nil, 0, fmt.Errorf("maskBody messages: %w", err)
	}

	total := 0
	for i, messageRaw := range messages {
		var message map[string]json.RawMessage
		if err := json.Unmarshal(messageRaw, &message); err != nil {
			return nil, 0, fmt.Errorf("maskBody message[%d]: %w", i, err)
		}
		content, ok := message["content"]
		if !ok {
			continue
		}
		masked, n, err := maskContent(content, f)
		if err != nil {
			return nil, 0, err
		}
		if n > 0 {
			message["content"] = masked
			remarshaled, err := json.Marshal(message)
			if err != nil {
				return nil, 0, fmt.Errorf("maskBody remarshal message[%d]: %w", i, err)
			}
			messages[i] = remarshaled
			total += n
		}
	}
	if total == 0 {
		return raw, 0, nil
	}
	newMessages, err := json.Marshal(messages)
	if err != nil {
		return nil, 0, fmt.Errorf("maskBody remarshal messages: %w", err)
	}
	top["messages"] = newMessages
	out, err := json.Marshal(top)
	if err != nil {
		return nil, 0, fmt.Errorf("maskBody remarshal: %w", err)
	}
	return out, total, nil
}

func maskContent(content json.RawMessage, f filter.RequestFilter) (json.RawMessage, int, error) {
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		masked, n := f.Mask(text)
		if n == 0 {
			return content, 0, nil
		}
		body, err := json.Marshal(masked)
		return body, n, err
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, 0, fmt.Errorf("maskContent: %w", err)
	}
	total := 0
	for i, blockRaw := range blocks {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			return nil, 0, fmt.Errorf("maskContent block[%d]: %w", i, err)
		}
		var typ string
		_ = json.Unmarshal(block["type"], &typ)
		if typ != "text" {
			continue
		}
		var text string
		if err := json.Unmarshal(block["text"], &text); err != nil {
			continue
		}
		masked, n := f.Mask(text)
		if n == 0 {
			continue
		}
		maskedBody, err := json.Marshal(masked)
		if err != nil {
			return nil, 0, err
		}
		block["text"] = maskedBody
		remarshaled, err := json.Marshal(block)
		if err != nil {
			return nil, 0, err
		}
		blocks[i] = remarshaled
		total += n
	}
	if total == 0 {
		return content, 0, nil
	}
	out, err := json.Marshal(blocks)
	return out, total, err
}
