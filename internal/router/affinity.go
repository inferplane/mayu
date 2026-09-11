package router

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/inferplane/inferplane/internal/policy"
)

const maxAffinityEntries = 4096

type affinityTarget struct {
	Model, Provider, Identity, Upstream, Region, Boundary string
}

func affinityTargetOf(ct ChainTarget) affinityTarget {
	return affinityTarget{ct.Model, ct.ProviderName, ct.Identity, ct.Upstream, ct.Region, ct.DataBoundary}
}

type affinityEntry struct {
	target         affinityTarget
	policy         [32]byte
	since, expires time.Time
	requests       int
	revision       uint64
}

type affinityStore struct {
	mu       sync.Mutex
	secret   [32]byte
	now      func() time.Time
	entries  map[[32]byte]affinityEntry
	revision uint64
}

func newAffinityStore() *affinityStore {
	s := &affinityStore{now: time.Now, entries: make(map[[32]byte]affinityEntry)}
	if _, err := rand.Read(s.secret[:]); err != nil {
		// Affinity is an optimization. No entropy means no pin, never a new
		// availability dependency or an unscoped digest of sensitive content.
		return nil
	}
	return s
}

// AffinityToken is an opaque, request-local capability for recording one actual
// successful attempt. It contains only digests and bounded configured targets;
// it must not be audited. A zero token cannot establish a pin.
type AffinityToken struct {
	owner       *affinityStore
	key, policy [32]byte
	expected    uint64
	issued      time.Time
	settings    policy.ContextStability
	allowed     []affinityTarget
	used        atomic.Bool
}

func (*AffinityToken) String() string { return "affinity-token" }

// SetAffinityClock supplies the local expiry clock. Startup-only, like the
// policy/inspector setters; nil restores the wall clock.
func (r *Router) SetAffinityClock(now func() time.Time) {
	if r.affinity == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	r.affinity.now = now
}

// RecordAffinitySuccess pins the actual successful target from the returned
// chain, not the proposed/planned model. Call once at successful completion or
// committed useful stream output; never for a terminal error or token count.
// Invalid, replayed, expired and superseded tokens are harmless no-ops.
func (r *Router) RecordAffinitySuccess(token *AffinityToken, target ChainTarget) {
	s := r.affinity
	if s == nil || token == nil || token.owner != s || !slices.Contains(token.allowed, affinityTargetOf(target)) {
		return
	}
	if !token.used.CompareAndSwap(false, true) {
		return
	}
	now := s.now()
	if now.Before(token.issued) || !now.Before(token.issued.Add(token.settings.SessionTTL)) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.entries[token.key]
	if exists && !now.Before(old.expires) {
		delete(s.entries, token.key)
		exists = false
	}
	if (exists && old.revision != token.expected) || (!exists && token.expected != 0) {
		return
	}
	if !exists && len(s.entries) >= maxAffinityEntries {
		for key, entry := range s.entries {
			if !now.Before(entry.expires) {
				delete(s.entries, key)
			}
		}
		if len(s.entries) >= maxAffinityEntries {
			return // capacity loss causes a cold cache, never a routing denial
		}
	}
	next := affinityEntry{target: affinityTargetOf(target), policy: token.policy, since: now, expires: now.Add(token.settings.SessionTTL), requests: 1}
	if exists && old.target == next.target && old.policy == next.policy {
		next.since = old.since
		next.requests = min(old.requests+1, 10000)
	}
	s.revision++
	next.revision = s.revision
	s.entries[token.key] = next
}

// stabilityOf intersects performance preferences: every matching context rule
// must opt in; hold constraints take the maximum and TTL the minimum.
func stabilityOf(rules []requestRule) *policy.ContextStability {
	var result *policy.ContextStability
	for _, rule := range rules {
		if rule.context == nil {
			continue
		}
		s := rule.context.Stability
		if s == nil || s.MinHold < 0 || s.MinRequests < 1 || s.MinRequests > 10000 ||
			s.SessionTTL <= 0 || s.SessionTTL > 24*time.Hour || s.SessionTTL < s.MinHold {
			return nil
		}
		if result == nil {
			copy := *s
			result = &copy
		} else {
			result.MinHold = max(result.MinHold, s.MinHold)
			result.MinRequests = max(result.MinRequests, s.MinRequests)
			result.SessionTTL = min(result.SessionTTL, s.SessionTTL)
		}
	}
	if result != nil && result.SessionTTL < result.MinHold {
		return nil
	}
	return result
}

func (s *affinityStore) sessionKey(in RequestRoutingInput) ([32]byte, bool) {
	h := hmac.New(sha256.New, s.secret[:])
	enc := json.NewEncoder(h)
	requested := in.RequestedModel
	if requested == "" {
		requested = in.Model
	}
	_ = enc.Encode([]string{in.Principal.Team, in.Principal.KeyID, in.Principal.Owner, in.Protocol, in.State.Canonical(requested)})
	if in.SessionHint != "" {
		if len(in.SessionHint) > 4096 {
			return [32]byte{}, false
		}
		_ = enc.Encode(in.SessionHint)
	} else {
		// Hash only the immutable first user content plus initial instructions/
		// tools. Appending turns must not change the key. Nothing is retained.
		var body struct {
			System   json.RawMessage `json:"system"`
			Tools    json.RawMessage `json:"tools"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Input        json.RawMessage `json:"input"`
			Instructions json.RawMessage `json:"instructions"`
		}
		if len(in.RawBody) > 64<<20 || json.Unmarshal(in.RawBody, &body) != nil {
			return [32]byte{}, false
		}
		_ = enc.Encode(body.System)
		_ = enc.Encode(body.Instructions)
		_ = enc.Encode(body.Tools)
		found := false
		for _, msg := range body.Messages {
			if msg.Role == "system" || msg.Role == "developer" {
				_ = enc.Encode(msg)
			}
			if msg.Role == "user" {
				_ = enc.Encode(msg.Content)
				found = true
				break
			}
		}
		if !found && in.Protocol == "responses" {
			var input string
			if json.Unmarshal(body.Input, &input) == nil {
				_ = enc.Encode(input)
				found = true
			} else {
				var items []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				}
				if json.Unmarshal(body.Input, &items) == nil {
					for _, item := range items {
						if item.Role == "user" {
							_ = enc.Encode(item.Content)
							found = true
							break
						}
					}
				}
			}
		}
		if !found {
			return [32]byte{}, false
		}
	}
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key, true
}

func affinityPolicyDigest(boundary requestBoundary, rules []requestRule, masked bool) [32]byte {
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode(boundary.in.Model)
	_ = enc.Encode(boundary.budgetModel)
	_ = enc.Encode(masked)
	_ = enc.Encode(boundary.models) // encoding/json sorts map keys
	for _, rule := range rules {
		_ = enc.Encode(rule.ref)
		_ = enc.Encode(rule.context)
		_ = enc.Encode(rule.sensitive)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func (r *Router) prepareAffinity(out RequestRoutingResult, boundary requestBoundary, rules []requestRule, settings *policy.ContextStability) (*AffinityToken, []ChainTarget) {
	s := r.affinity
	if s == nil || settings == nil || boundary.in.CountOnly {
		return nil, nil
	}
	key, ok := s.sessionKey(boundary.in)
	if !ok {
		return nil, nil
	}
	now := s.now()
	digest := affinityPolicyDigest(boundary, rules, out.Decision.Masked)
	s.mu.Lock()
	entry, exists := s.entries[key]
	if exists && (!now.Before(entry.expires) || entry.policy != digest) {
		delete(s.entries, key)
		exists = false
	}
	s.mu.Unlock()
	token := &AffinityToken{owner: s, key: key, policy: digest, issued: now, settings: *settings}
	if !exists {
		return token, nil
	}
	token.expected = entry.revision
	chain := r.requestChain(boundary, entry.target.Model)
	// A stored automatic selection never inherits the original model's
	// metadata exemption. Every hit rechecks capacity/capabilities/pricing.
	chain = boundary.filter(chain, true)
	for i, target := range chain {
		if affinityTargetOf(target) == entry.target && r.brk.Allow(target.Identity) {
			if now.Before(entry.since.Add(settings.MinHold)) || entry.requests < settings.MinRequests {
				chain = append([]ChainTarget{target}, append(chain[:i:i], chain[i+1:]...)...)
				return token, chain
			}
			return token, nil
		}
	}
	// Remove invalid targets immediately. The next successful permitted
	// destination can establish a new pin; stale requests cannot overwrite it.
	s.mu.Lock()
	if current, ok := s.entries[key]; ok && current.revision == entry.revision {
		delete(s.entries, key)
		token.expected = 0
	}
	s.mu.Unlock()
	return token, nil
}

func finishAffinity(out RequestRoutingResult, token *AffinityToken, boundary requestBoundary) RequestRoutingResult {
	if token == nil {
		return out
	}
	for _, target := range boundary.filter(out.Chain, true) {
		token.allowed = append(token.allowed, affinityTargetOf(target))
	}
	if len(token.allowed) > 0 {
		out.AffinityToken = token
	}
	return out
}
