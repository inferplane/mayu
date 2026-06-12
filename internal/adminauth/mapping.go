// Package adminauth is the admin-plane identity leaf: the OIDC bearer-shape
// predicate shared by config validation and the auth middleware (one symbol —
// the two can never drift, which is what keeps the break-glass static token
// from ever being routed to the OIDC path), and the groups→team mapping
// resolver (§5.1: the gateway owns only the mapping rules; identity itself is
// the IdP's). Depends only on stdlib (+ go-oidc in oidc.go) so config and
// server can both import it without cycles.
package adminauth

// maxBearerLen caps how large a bearer token the admin plane will even look
// at — an oversized Authorization header is rejected before any splitting or
// parsing (DoS guard, plan r3).
const maxBearerLen = 8 * 1024

// IsOIDCBearerShape reports whether bearer looks like a JWS compact
// serialization: exactly three dot-separated, non-empty, base64url (no
// padding) segments, within the size cap. This is the ONLY definition of
// "JWT-shaped" in the codebase: the config loader uses it to reject static
// admin tokens that would be mis-routed, and AdminAuth uses it to route.
// JWE (5 segments), padded, or otherwise non-conforming bearers are NOT
// JWT-shaped and take the static-token path.
func IsOIDCBearerShape(bearer string) bool {
	if bearer == "" || len(bearer) > maxBearerLen {
		return false
	}
	seg := 0
	start := 0
	for i := 0; i <= len(bearer); i++ {
		if i == len(bearer) || bearer[i] == '.' {
			if i == start { // empty segment (leading/trailing/double dot)
				return false
			}
			seg++
			if seg > 3 {
				return false
			}
			start = i + 1
			continue
		}
		if !isBase64URLByte(bearer[i]) {
			return false
		}
	}
	return seg == 3
}

func isBase64URLByte(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '-' || b == '_':
		return true
	default:
		return false
	}
}

// MappingConfig is the adminauth-side shape of the groups→team rules —
// decoupled from internal/config (same pattern as governance.ConfigTeam) so
// this package stays a leaf.
type MappingConfig struct {
	// AdminGroups grant IsAdmin (all teams, key issuance for any team).
	AdminGroups []string
	// GroupMappings map one IdP group to one or more teams. The literal
	// group "*" matches every authenticated identity that has at least one
	// group; it is an explicit opt-in, distinct from "no mapping matched".
	GroupMappings []GroupMapping
}

// GroupMapping maps a single IdP group to gateway teams.
type GroupMapping struct {
	Group string
	Teams []string
}

// Resolve maps a verified identity's groups onto gateway teams. Admin-group
// membership wins (isAdmin=true, teams nil — admins are entitled to every
// team). Otherwise the result is the deduplicated union of all matching
// mappings, in mapping-config order. ok=false means the identity mapped to
// nothing — the caller must treat that as authenticated-but-unauthorized
// (403), never as a default team (silent privilege grant is banned).
func Resolve(groups []string, cfg MappingConfig) (teams []string, isAdmin, ok bool) {
	if len(groups) == 0 {
		return nil, false, false
	}
	inGroups := map[string]bool{}
	for _, g := range groups {
		inGroups[g] = true
	}
	for _, ag := range cfg.AdminGroups {
		if inGroups[ag] {
			return nil, true, true
		}
	}
	seen := map[string]bool{}
	for _, m := range cfg.GroupMappings {
		if m.Group != "*" && !inGroups[m.Group] {
			continue
		}
		for _, t := range m.Teams {
			if !seen[t] {
				seen[t] = true
				teams = append(teams, t)
			}
		}
	}
	if len(teams) == 0 {
		return nil, false, false
	}
	return teams, false, true
}
