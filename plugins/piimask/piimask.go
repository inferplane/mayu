// Package piimask is the opt-in PII masking request filter (ADR-009). It
// replaces detected PII in request TEXT with typed placeholders before the
// request is forwarded upstream — one-way (no vault, no un-masking), so the
// gateway never stores PII. It registers itself as "pii-mask" via init(); the
// gateway blank-imports it (like a provider) and the assembly injects it into
// the request handlers per the `plugins` config.
//
// Masking mutates the body, which abandons verbatim RawBody forwarding and so
// destroys the prompt cache for masked traffic — that cost is opt-in and made
// explicit by the caller (warning + metric + audit, ADR-009 §4.4 / §308).
package piimask

import (
	"regexp"
	"strings"

	"github.com/inferplane/inferplane/internal/filter"
)

// Placeholders are typed so the model still sees the SHAPE of the redacted span.
const (
	phEmail = "‹EMAIL›"
	phPhone = "‹PHONE›"
	phCard  = "‹CARD›"
	phSSN   = "‹SSN›"
	phIP    = "‹IP›"
)

var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reSSN   = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	reIP    = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// candidate card: 13–19 digits, separators BETWEEN digits only (so a trailing
	// space is never consumed); ends on a digit. Luhn-gated below.
	reCard = regexp.MustCompile(`\b\d(?:[ \-]?\d){12,18}\b`)
	// NA / E.164-ish phone: optional +1, area code, 7 digits with separators.
	rePhone = regexp.MustCompile(`\+?1?[\s.\-]?\(?\d{3}\)?[\s.\-]?\d{3}[\s.\-]?\d{4}\b`)
)

// Options toggles individual detectors (all enabled by default — the zero value
// masks everything).
type Options struct {
	DisableEmail bool
	DisablePhone bool
	DisableCard  bool
	DisableSSN   bool
	DisableIP    bool
}

// Masker is the pii-mask filter.
type Masker struct{ opt Options }

// New builds a Masker with the given detector toggles.
func New(opt Options) *Masker { return &Masker{opt: opt} }

func (m *Masker) Name() string { return "pii-mask" }

// Mask applies the enabled detectors in specificity order and returns the masked
// text plus the total number of substitutions. Card is checked with Luhn (cuts
// false positives); the others are regex. Order matters: the most specific /
// structured patterns run before the greedier phone pattern.
func (m *Masker) Mask(text string) (string, int) {
	n := 0
	count := func(re *regexp.Regexp, ph string, enabled bool) {
		if !enabled {
			return
		}
		text = re.ReplaceAllStringFunc(text, func(string) string { n++; return ph })
	}
	// email & ssn first (distinct shapes), then card (Luhn-gated), then ip, then phone.
	count(reEmail, phEmail, !m.opt.DisableEmail)
	count(reSSN, phSSN, !m.opt.DisableSSN)
	if !m.opt.DisableCard {
		text = reCard.ReplaceAllStringFunc(text, func(s string) string {
			if luhnValid(digitsOnly(s)) {
				n++
				return phCard
			}
			return s // not a valid card number — leave it
		})
	}
	count(reIP, phIP, !m.opt.DisableIP)
	count(rePhone, phPhone, !m.opt.DisablePhone)
	return text, n
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// luhnValid runs the Luhn checksum; a card candidate is masked only if it passes
// (and is a plausible card length), which removes most non-card digit runs.
func luhnValid(digits string) bool {
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

func init() { filter.Register(New(Options{})) }
