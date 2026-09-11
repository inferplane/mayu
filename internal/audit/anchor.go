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

// AnchorReader reads back the most recently witnessed AnchorPoint for one
// instance. An Anchorer need not implement it — writing to WORM storage
// doesn't require the ability to list it back — so this is a SEPARATE,
// optional interface a concrete anchorer may also satisfy. nil, nil means no
// anchor has been witnessed yet for that instance (never treated as tamper
// evidence — a fresh instance or an anchorer that only just started has none).
//
// Without a reader, /admin/audit/verify can only prove the file's OWN internal
// consistency: a whole-file replacement starting fresh from genesis, or a
// truncated tail, both verify as OK with nothing to say a record is missing.
// The anchor is the external witness that closes that gap — but only once
// something actually reads it back and compares.
type AnchorReader interface {
	Latest(ctx context.Context, instance string) (*AnchorPoint, error)
}
