package audit

import "context"

// AnchorPoint is the immutable witness written to WORM storage (ADR-012): the
// audit chain head at an instant. It carries NO secret/PII — only the instance
// id (operator-chosen, should be opaque), the chain head hash, the record count,
// and the timestamp.
type AnchorPoint struct {
	Instance string `json:"instance"`
	HeadHash string `json:"head_hash"`
	Count    int64  `json:"count"`
	TS       string `json:"ts"` // RFC3339Nano UTC
}

// Anchorer writes an AnchorPoint to an immutable external store (e.g. S3 Object
// Lock). Implementations are best-effort: the caller (anchor worker) logs and
// retries on error, never failing request serving (ADR-012).
type Anchorer interface {
	Anchor(ctx context.Context, p AnchorPoint) error
}
