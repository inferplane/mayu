package responsesapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/pricing"
	"github.com/inferplane/inferplane/internal/responses"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/requestpolicy"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type attempt struct {
	h         *Handler
	req       *http.Request
	principal keystore.Principal
	target    router.ChainTarget
	proxy     *providers.ProxyRequest
	table     *pricing.Table
	started   time.Time
	estimate  int64
	inputBody []byte // sanitized client wire; proxy.RawBody may be translated
}

func errorStatus(err error) int {
	var upstream *providers.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.HTTPStatus()
	}
	return http.StatusBadGateway
}
func retryStatus(status int) bool { return status == 429 || status >= 500 }

func (a attempt) complete(w http.ResponseWriter, last bool) bool {
	resp, err := a.target.Provider.Complete(a.req.Context(), a.proxy)
	if err != nil || resp == nil {
		status := errorStatus(err)
		a.finish(status, nil, nil, false, 0)
		if !last && retryStatus(status) && a.req.Context().Err() == nil {
			return true
		}
		writeError(w, status, "upstream_error", "upstream request failed")
		return false
	}
	if resp.StatusCode/100 != 2 {
		var observed *schema.Usage
		if resp.Parsed != nil {
			observed = resp.Parsed.Usage
		}
		a.finish(resp.StatusCode, observed, nil, false, 0)
		if !last && retryStatus(resp.StatusCode) {
			return true
		}
		writeError(w, resp.StatusCode, "upstream_error", "upstream request failed")
		return false
	}
	if resp.Parsed == nil || resp.Parsed.Usage == nil {
		a.finish(502, nil, nil, false, 0)
		writeError(w, 502, "upstream_error", "upstream response omitted usage")
		return false
	}
	body := resp.RawBody
	if a.target.Provider.Name() != "openai_responses" {
		body, err = responses.CanonicalToResponseForRequest(resp.Parsed, a.proxy.Parsed)
	}
	if err != nil || len(body) == 0 {
		// A provider successfully processed this request. Settle its observed
		// usage even if the output cannot be rendered; do not bill a retry.
		a.finish(502, resp.Parsed.Usage, nil, false, 0)
		writeError(w, 502, "upstream_error", "upstream response is incompatible")
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, writeErr := w.Write(body)
	a.h.r.RecordResult(a.target.ProviderName, a.target.Identity, true)
	if writeErr == nil {
		requestpolicy.RecordSuccess(a.req)
	}
	a.finish(resp.StatusCode, resp.Parsed.Usage, body, writeErr != nil, 0)
	return false
}

func (a attempt) stream(w http.ResponseWriter, last bool) bool {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "api_error", "streaming is unavailable")
		a.finish(500, nil, nil, false, 0)
		return false
	}
	seq, err := a.target.Provider.Stream(a.req.Context(), a.proxy)
	if err != nil || seq == nil {
		status := errorStatus(err)
		a.finish(status, nil, nil, false, 0)
		if !last && retryStatus(status) && a.req.Context().Err() == nil {
			return true
		}
		writeError(w, status, "upstream_error", "upstream stream failed")
		return false
	}
	native := a.target.Provider.Name() == "openai_responses"
	renderer := responses.NewStreamState(a.target.Model, a.proxy.Parsed)
	var usage *schema.Usage
	var ttft float64
	var conversionErr error
	committed, terminal, failed := false, false, false
	commit := func() {
		if !committed {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(200)
			committed = true
			ttft = time.Since(a.started).Seconds()
		}
	}
	fail := func(cause error) bool {
		if !committed {
			status := errorStatus(cause)
			a.finish(status, usage, nil, false, ttft)
			if conversionErr == nil && !last && retryStatus(status) && a.req.Context().Err() == nil {
				return true
			}
			writeError(w, status, "upstream_error", "upstream stream failed")
			return false
		}
		// Never expose arbitrary provider error strings or restart a committed
		// stream on another model. Output already observed is still settled.
		_ = responses.WriteEvent(w, responses.Event{Type: "error", Data: json.RawMessage(`{"type":"error","code":"upstream_error","message":"upstream stream interrupted"}`)})
		flusher.Flush()
		a.finish(200, usage, nil, true, ttft)
		return false
	}
	for event, readErr := range seq {
		if readErr != nil {
			return fail(readErr)
		}
		if event == nil {
			continue
		}
		if event.Chunk != nil {
			if event.Chunk.Message != nil {
				usage = schema.MergeUsage(usage, event.Chunk.Message.Usage)
			}
			usage = schema.MergeUsage(usage, event.Chunk.Usage)
			terminal = terminal || event.Chunk.Type == "message_stop"
			failed = failed || event.Chunk.Type == "error"
		}
		if conversionErr != nil {
			// A conversion failure can precede the terminal usage frame (for
			// example a truncated tool argument at a synthesized block-stop).
			// Keep consuming accounting under the same cancellable request;
			// never emit a successful completion or start another attempt.
			continue
		}
		if native {
			if len(event.Raw) > 0 {
				commit()
				if _, err := w.Write(event.Raw); err != nil {
					a.finish(200, usage, nil, true, ttft)
					return false
				}
				flusher.Flush()
			}
		} else if event.Chunk != nil {
			events, err := renderer.Convert(event.Chunk)
			if err != nil {
				conversionErr = err
				continue
			}
			for _, converted := range events {
				commit()
				if err := responses.WriteEvent(w, converted); err != nil {
					a.finish(200, usage, nil, true, ttft)
					return false
				}
			}
			if len(events) > 0 {
				flusher.Flush()
			}
		}
	}
	if conversionErr != nil {
		return fail(conversionErr)
	}
	if !native {
		if _, err := renderer.Finish(); err != nil {
			return fail(err)
		}
	}
	if !committed || !terminal || usage == nil || failed {
		return fail(errors.New("incomplete stream"))
	}
	a.h.r.RecordResult(a.target.ProviderName, a.target.Identity, true)
	requestpolicy.RecordSuccess(a.req)
	a.finish(200, usage, nil, false, ttft)
	return false
}
