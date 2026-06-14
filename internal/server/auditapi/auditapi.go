// Package auditapi serves the admin-plane audit-chain verification endpoint
// (ADR-003 #2): GET /admin/audit/verify runs the tamper-evident hash-chain
// check over each configured file sink and returns a secret-free per-sink
// result. It is mounted behind AdminAuth (read-only, no record contents
// returned). The chain is verified offline by `inferplane audit verify` too;
// this is the one-click operator view.
package auditapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"

	"github.com/inferplane/inferplane/internal/audit"
)

// maxVerifyBytes caps synchronous on-demand verification. A larger file is not
// scanned in-request (AdminAuth is not a DoS control) — the operator uses the
// offline `inferplane audit verify` CLI instead.
const maxVerifyBytes = 16 << 20 // 16 MiB

// SinkResult is the verification outcome for one file sink. No record contents,
// no secrets — only the chain verdict.
type SinkResult struct {
	Path        string `json:"path"`
	OK          bool   `json:"ok"`
	Records     int    `json:"records,omitempty"`
	BrokenAt    int    `json:"broken_at,omitempty"`
	Reason      string `json:"reason,omitempty"`
	PartialTail bool   `json:"partial_tail,omitempty"`
}

type response struct {
	Sinks []SinkResult `json:"sinks"`
}

// Handler verifies each path in paths on GET. Writes return 405. paths is the
// set of configured `file` audit-sink paths (empty ⇒ {"sinks":[]}).
func Handler(paths []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		out := response{Sinks: make([]SinkResult, 0, len(paths))}
		for _, p := range paths {
			out.Sinks = append(out.Sinks, verifyFile(p))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// verifyFile checks one file sink: it rejects non-regular files (rotation /
// symlink-swap safety), caps the size, reads the COMPLETE prefix (up to the
// last newline — a partial trailing line from a live writer is trimmed, not
// treated as tampering, and flagged via PartialTail), and runs audit.Verify.
func verifyFile(path string) SinkResult {
	res := SinkResult{Path: path}
	fi, err := os.Stat(path)
	if err != nil {
		res.Reason = "stat failed: " + err.Error()
		return res
	}
	if !fi.Mode().IsRegular() {
		res.Reason = "not a regular file"
		return res
	}
	if fi.Size() > maxVerifyBytes {
		res.Reason = "too large for online verify; use `inferplane audit verify`"
		return res
	}
	data, err := os.ReadFile(path)
	if err != nil {
		res.Reason = "read failed: " + err.Error()
		return res
	}
	// Verify only the complete, newline-terminated prefix. Anything after the
	// last newline is an in-flight partial write — never verified, never
	// claimed as tampering.
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		if i+1 != len(data) {
			res.PartialTail = true
		}
		data = data[:i+1]
	} else if len(data) > 0 {
		// No complete line yet (only a partial first line).
		res.PartialTail = true
		data = nil
	}
	vr, err := audit.Verify(bytes.NewReader(data))
	if err != nil {
		res.Reason = "verify error: " + err.Error()
		return res
	}
	res.OK = vr.OK
	res.Records = vr.Records
	res.BrokenAt = vr.BrokenAt
	if !vr.OK {
		res.Reason = vr.Reason
	}
	return res
}
