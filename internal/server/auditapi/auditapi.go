// Package auditapi serves the admin-plane audit-chain verification endpoint
// (ADR-003 #2): GET /admin/audit/verify runs the tamper-evident hash-chain
// check over each configured file sink and returns a secret-free per-sink
// result. It is mounted behind AdminAuth (read-only, no record contents
// returned). The chain is verified offline by `mayu audit verify` too;
// this is the one-click operator view.
package auditapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"

	"github.com/inferplane/inferplane/internal/audit"
)

// maxVerifyBytes caps synchronous on-demand verification. A larger file is not
// scanned in-request (AdminAuth is not a DoS control) — the operator uses the
// offline `mayu audit verify` CLI instead.
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
	// AnchorChecked is true only when an external anchor (ADR-012) was
	// actually consulted for this sink's instance. Its absence — no
	// AnchorReader configured, or nothing anchored yet — is NOT itself a
	// finding: OK==true with AnchorChecked==false means "internally
	// consistent, but nothing outside the file itself was compared."
	AnchorChecked bool `json:"anchor_checked,omitempty"`
}

type response struct {
	Sinks []SinkResult `json:"sinks"`
}

// Handler verifies each path in paths on GET. Writes return 405. paths is the
// set of configured `file` audit-sink paths (empty ⇒ {"sinks":[]}).
//
// reader and instance are the external-anchor cross-check (ADR-012): reader
// may be nil (no anchorer configured, or the configured one doesn't implement
// audit.AnchorReader — s3anchor does) — Verify's internal-consistency check
// still runs, just without the cross-check. instance is the CURRENT writer's
// chain segment (audit.Writer.Instance()); an anchor from any other instance
// is out of scope for this file's live tail.
func Handler(paths []string, reader audit.AnchorReader, instance string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		out := response{Sinks: make([]SinkResult, 0, len(paths))}
		for _, p := range paths {
			out.Sinks = append(out.Sinks, verifyFile(r.Context(), p, reader, instance))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// verifyFile checks one file sink: it rejects non-regular files (rotation /
// symlink-swap safety), caps the size, reads the COMPLETE prefix (up to the
// last newline — a partial trailing line from a live writer is trimmed, not
// treated as tampering, and flagged via PartialTail), and runs audit.Verify —
// then, when reader is non-nil, cross-checks the result against the latest
// external anchor for instance.
func verifyFile(ctx context.Context, path string, reader audit.AnchorReader, instance string) SinkResult {
	res := SinkResult{Path: path}
	// Open ONCE and stat/read the same descriptor, so the type and size checks
	// can't be raced by a rotation/symlink-swap between a separate Stat and
	// ReadFile (TOCTOU). The size cap is enforced by reading through a
	// LimitReader of cap+1 — a file that grows past the cap after fstat is
	// still bounded.
	f, err := os.Open(path)
	if err != nil {
		res.Reason = "open failed: " + err.Error()
		return res
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		res.Reason = "stat failed: " + err.Error()
		return res
	}
	if !fi.Mode().IsRegular() {
		res.Reason = "not a regular file"
		return res
	}
	data, err := io.ReadAll(io.LimitReader(f, maxVerifyBytes+1))
	if err != nil {
		res.Reason = "read failed: " + err.Error()
		return res
	}
	if len(data) > maxVerifyBytes {
		res.Reason = "too large for online verify; use `mayu audit verify`"
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
	if res.OK && reader != nil {
		checkAnchor(ctx, &res, data, vr, reader, instance)
	}
	return res
}

// checkAnchor cross-checks an internally-consistent chain against the latest
// external anchor for instance. Mutates res in place — on any mismatch it
// flips OK to false, exactly the same tamper-evidence signal a broken
// prev_hash produces, because from the operator's chair "the file is
// internally consistent but disagrees with what was witnessed outside it" IS
// tamper evidence: a truncated tail or a whole-file replacement (fresh
// genesis) both verify clean on their own.
func checkAnchor(ctx context.Context, res *SinkResult, data []byte, vr audit.VerifyResult, reader audit.AnchorReader, instance string) {
	anchor, err := reader.Latest(ctx, instance)
	if err != nil {
		// The anchor store being unreachable must not fail a request that
		// otherwise verified clean — ADR-012's anchoring is best-effort by
		// design, and treating an S3 outage as tamper evidence would page
		// someone for the wrong reason. AnchorChecked stays false so the
		// operator can see the cross-check did not actually run.
		return
	}
	if anchor == nil {
		return // nothing anchored yet for this instance: not evidence of anything
	}
	res.AnchorChecked = true
	st := vr.Instances[instance]
	switch {
	case st.Count < anchor.Count:
		res.OK = false
		res.Reason = "chain has fewer records than the last external anchor witnessed — possible truncation"
	case st.Count == anchor.Count:
		if st.HeadHash != anchor.HeadHash {
			res.OK = false
			res.Reason = "chain head does not match the last external anchor at the same record count — tampering"
		}
	default: // st.Count > anchor.Count: recompute the chain's state at the anchor's exact position
		got, found, err := audit.HeadAtCount(bytes.NewReader(data), instance, anchor.Count)
		if err != nil {
			return // read error, not a verdict — same posture as the Latest() error above
		}
		if !found || got != anchor.HeadHash {
			res.OK = false
			res.Reason = "chain's recorded state at the last external anchor's position does not match the anchor — tampering"
		}
	}
}
