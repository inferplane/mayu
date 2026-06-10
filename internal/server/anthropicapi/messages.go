// Package anthropicapi implements the Anthropic-shaped ingress endpoints.
package anthropicapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type MessagesHandler struct{ r *router.Router }

func NewMessagesHandler(r *router.Router) *MessagesHandler { return &MessagesHandler{r: r} }

func (h *MessagesHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, 400, "invalid_request_error", "could not read request body")
		return
	}
	// Parse for routing/observation ONLY. RawBody is forwarded verbatim.
	var parsed schema.ChatRequest
	if err := json.Unmarshal(raw, &parsed); err != nil {
		writeErr(w, 400, "invalid_request_error", "malformed JSON")
		return
	}
	prov, upstream, err := h.r.Resolve(parsed.Model)
	if err != nil {
		writeErr(w, 404, "not_found_error", "unknown model: "+parsed.Model)
		return
	}
	stream := parsed.Stream != nil && *parsed.Stream
	pr := &providers.ProxyRequest{
		Model: parsed.Model, Upstream: upstream, Parsed: &parsed,
		RawBody: raw, Headers: req.Header, Stream: stream,
	}
	if stream {
		h.serveStream(w, req, prov, pr)
		return
	}
	h.serveComplete(w, req, prov, pr)
}

func (h *MessagesHandler) serveComplete(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest) {
	resp, err := prov.Complete(req.Context(), pr)
	if err != nil {
		writeErr(w, 502, "api_error", "upstream error")
		return
	}
	if resp.Headers != nil {
		copyUpstreamHeaders(w.Header(), resp.Headers)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.RawBody) // tee verbatim (incl. non-2xx error bodies)
	// resp.Parsed.Usage is the observation hook for M3 audit / M5 quota.
}

func (h *MessagesHandler) serveStream(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest) {
	seq, err := prov.Stream(req.Context(), pr)
	if err != nil {
		// Tee a non-2xx upstream error verbatim (status/body) so the client
		// sees Anthropic's real rate-limit/error response, not a fabricated one.
		var ue *providers.UpstreamError
		if errors.As(err, &ue) {
			copyUpstreamHeaders(w.Header(), ue.Header)
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(ue.StatusCode)
			w.Write(ue.Body)
			return
		}
		writeErr(w, 502, "api_error", "upstream stream error")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "api_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	for ev, err := range seq {
		if err != nil {
			return // upstream broke mid-stream; client sees truncated stream (M5: error event)
		}
		w.Write(ev.Raw) // tee original bytes verbatim
		flusher.Flush()
		// ev.Chunk.Usage on message_delta is the settlement observation point (M3/M5).
	}
}

// copyUpstreamHeaders tees upstream response headers to the client, skipping
// hop-by-hop headers Go manages itself. Preserves request-id and
// anthropic-ratelimit-*/retry-after so the client keeps its backoff signal.
func copyUpstreamHeaders(dst http.Header, src http.Header) {
	for k, vs := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Transfer-Encoding", "Connection":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func writeErr(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
