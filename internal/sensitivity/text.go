package sensitivity

import (
	"context"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var detectors = []struct {
	category string
	pattern  *regexp.Regexp
	valid    func(string) bool
}{
	{"email", regexp.MustCompile("(?i)[a-z0-9.!#$%&'*+/=?^_`{|}~-]{1,64}@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?){1,8}"), nil},
	{"ssn", regexp.MustCompile(`[0-9]{3}-[0-9]{2}-[0-9]{4}`), validSSN},
	{"ipv4", regexp.MustCompile(`[0-9]{1,3}(?:\.[0-9]{1,3}){3}`), func(s string) bool {
		addr, err := netip.ParseAddr(s)
		return err == nil && addr.Is4()
	}},
	{"korean_rrn", regexp.MustCompile(`[0-9]{6}-[1-8][0-9]{6}`), func(s string) bool {
		month, _ := strconv.Atoi(s[2:4])
		day, _ := strconv.Atoi(s[4:6])
		return month >= 1 && month <= 12 && day >= 1 && day <= 31
	}},
	{"phone", regexp.MustCompile(`(?:\+?1[ .-]?)?(?:\([2-9][0-9]{2}\)|[2-9][0-9]{2})[ .-][2-9][0-9]{2}[ .-][0-9]{4}`), nil},
	{"phone", regexp.MustCompile(`(?:\+82[ -]?(?:10|2|[3-6][1-5])|0(?:10|2|[3-6][1-5]))[ -][0-9]{3,4}[ -][0-9]{4}`), nil},
	{"phone", regexp.MustCompile(`\+[1-9][0-9]{0,2}(?:[ -][0-9]{1,4}){2,5}`), func(s string) bool {
		n := len(digits(s))
		return n >= 8 && n <= 15
	}},
	{"phone", regexp.MustCompile(`\+[1-9][0-9]{7,14}`), nil},
}

func detect(ctx context.Context, text string, found map[string]bool) error {
	const chunk, overlap = 8192, 1024 // overlap exceeds every detector's maximum width
	for start := 0; start < len(text); start += chunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+chunk+overlap, len(text))
		if !found["credit_card"] && containsCard(text, start, end) {
			found["credit_card"] = true
		}
		for _, detector := range detectors {
			if found[detector.category] {
				continue
			}
			for _, index := range detector.pattern.FindAllStringIndex(text[start:end], -1) {
				left, right := start+index[0], start+index[1]
				if end < len(text) && right == end {
					continue // a truncated candidate is handled in the next chunk
				}
				if detector.category != "email" && !digitBoundary(text, left, right) {
					continue
				}
				if detector.valid == nil || detector.valid(text[left:right]) {
					found[detector.category] = true
					break
				}
			}
		}
	}
	return nil
}

func containsCard(text string, start, end int) bool {
	// Try every digit-boundary start and every supported length. A rejected
	// candidate must not swallow a later card or part of an adjacent card.
	// At most 19 digits (37 bytes including separators) are examined per start.
	for left := start; left < end; left++ {
		if text[left] < '0' || text[left] > '9' ||
			(left > 0 && text[left-1] >= '0' && text[left-1] <= '9') {
			continue
		}
		right := left
		for count := 1; count <= 19 && right < end; count++ {
			if text[right] < '0' || text[right] > '9' {
				break
			}
			right++
			if count >= 13 && digitBoundary(text, left, right) && validCard(text[left:right]) {
				return true
			}
			if right < end && (text[right] == ' ' || text[right] == '-') {
				right++
			}
		}
	}
	return false
}

func digitBoundary(text string, left, right int) bool {
	return (left == 0 || text[left-1] < '0' || text[left-1] > '9') &&
		(right == len(text) || text[right] < '0' || text[right] > '9')
}

func digits(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := range len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i]-'0')
		}
	}
	return out
}

func validCard(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum, nonzero := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i])
		nonzero = nonzero || n != 0
		if (len(d)-1-i)%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return nonzero && sum%10 == 0
}

func validSSN(s string) bool {
	area, _ := strconv.Atoi(s[:3])
	return area != 0 && area != 666 && area < 900 && s[4:6] != "00" && s[7:] != "0000"
}

// MatchesKeywords matches nonempty substrings of decoded JSON strings and keys
// using Unicode simple case folding. Borrowed arguments are not modified.
// Invalid, ambiguous, or over-limit JSON never yields a partial positive result.
// Shape/completeness decisions and errors belong to Inspector.Inspect.
func MatchesKeywords(raw []byte, keywords []string) bool {
	var folded []string
	for _, keyword := range keywords {
		if keyword != "" {
			folded = append(folded, fold(keyword))
		}
	}
	if len(folded) == 0 {
		return false
	}
	matched := false
	w := walker{ctx: context.Background(), visit: func(text string) error {
		if !matched {
			text = fold(text)
			for _, keyword := range folded {
				if strings.Contains(text, keyword) {
					matched = true
					break
				}
			}
		}
		return nil
	}}
	root, err := w.decode(raw, 0)
	if err == nil {
		var shape Result
		err = shape.inspectShape("openai", root)
	}
	return err == nil && matched
}

func fold(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for _, r := range text {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		out.WriteRune(minimum)
	}
	return out.String()
}
