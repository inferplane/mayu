package schema

// MergeUsage folds a newly-observed Usage into a running one and returns the
// result. Each field takes the latest NON-NIL value seen — Anthropic refines
// and repeats counts across streaming frames rather than adding to them, so
// this is a fold, not a sum (summing would double-bill any count that appears
// in more than one frame).
//
// Why this exists (ADR-030): the settlement path used to keep only the LAST
// frame's top-level `usage`, which on the Anthropic wire is `message_delta` —
// and a `message_delta` commonly carries `output_tokens` alone. The input and
// prompt-cache counts live on `message_start`, nested under `message.usage`
// (schema.ChatChunk.Message.Usage). Overwriting instead of folding therefore
// billed streaming requests for output only, silently dropping every input and
// cache token. Both frames must be folded in, in arrival order.
//
// A nil acc returns a copy of next (never next itself — callers keep folding
// into the result and must not mutate the provider's frame). A nil next leaves
// acc unchanged. Extra is carried from acc, else next: it is passthrough
// preservation for round-tripping, never a billed field.
func MergeUsage(acc, next *Usage) *Usage {
	if next == nil {
		return acc
	}
	if acc == nil {
		cp := *next
		cp.CacheCreation = mergeCacheCreation(nil, next.CacheCreation)
		return &cp
	}
	out := *acc
	out.AccountingUncertain = acc.AccountingUncertain || next.AccountingUncertain
	if next.InputTokens != nil {
		out.InputTokens = next.InputTokens
	}
	if next.OutputTokens != nil {
		out.OutputTokens = next.OutputTokens
	}
	if next.CacheReadInputTokens != nil {
		out.CacheReadInputTokens = next.CacheReadInputTokens
	}
	if next.CacheCreationInputTokens != nil {
		out.CacheCreationInputTokens = next.CacheCreationInputTokens
	}
	out.CacheCreation = mergeCacheCreation(acc.CacheCreation, next.CacheCreation)
	if out.Extra == nil {
		out.Extra = next.Extra
	}
	return &out
}

// CacheWriteTiers resolves cache-CREATION tokens into the 5-minute and 1-hour
// tiers that pricing bills separately (5m = 1.25x the input rate, 1h = 2x).
//
// Two wire shapes carry the same information and MUST NOT be added together:
//   - `cache_creation: {ephemeral_5m_input_tokens, ephemeral_1h_input_tokens}`
//     — the TTL-split form. Authoritative when present.
//   - `cache_creation_input_tokens` — a flat total that predates the split.
//     Used only as a fallback, attributed to the cheaper 5m tier so an
//     unknown mix is never over-billed.
//
// Summing both would double-count every cache write, since a provider that
// sends the split generally also sends the flat total.
//
// This lives here, on the wire type, because the mapping was previously
// open-coded at six call sites (settle + observeTokens across three ingress
// handlers) and every one of them dropped the 1h tier — see ADR-030.
func (u *Usage) CacheWriteTiers() (write5m, write1h int64) {
	if u == nil {
		return 0, 0
	}
	if u.CacheCreation != nil {
		w5 := deref64(u.CacheCreation.Ephemeral5mInputTokens)
		w1 := deref64(u.CacheCreation.Ephemeral1hInputTokens)
		if w5 != 0 || w1 != 0 {
			return w5, w1
		}
		// An all-zero split is not evidence of no cache write — fall through
		// to the flat total rather than reporting zero over it.
	}
	return deref64(u.CacheCreationInputTokens), 0
}

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// mergeCacheCreation folds the TTL-split cache-write counts with the same
// latest-non-nil rule. The two tiers are priced differently (1h is 2x input,
// 5m is 1.25x), so they must stay separate all the way to pricing.Usage —
// collapsing them is the mis-billing this split exists to prevent.
func mergeCacheCreation(acc, next *CacheCreation) *CacheCreation {
	if next == nil {
		return acc
	}
	if acc == nil {
		cp := *next
		return &cp
	}
	out := *acc
	if next.Ephemeral5mInputTokens != nil {
		out.Ephemeral5mInputTokens = next.Ephemeral5mInputTokens
	}
	if next.Ephemeral1hInputTokens != nil {
		out.Ephemeral1hInputTokens = next.Ephemeral1hInputTokens
	}
	if out.Extra == nil {
		out.Extra = next.Extra
	}
	return &out
}
