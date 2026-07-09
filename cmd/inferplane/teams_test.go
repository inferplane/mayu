package main

// D3 (ADR-016) end-to-end: teams as first-class keystore records, with
// hot-path enforcement wired dynamically (Governor.SetTeamLookup) — an admin
// console edit must take effect on the very next request, no restart, no
// hot-reload trigger.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// teamsAPIConfig mirrors govConfig's pricing override (so a request has a
// real, nonzero cost to debit) but declares NO config teams — every team in
// this test exists only via the /admin/teams API, or not at all.
func teamsAPIConfig(upstreamURL string) func(cfg map[string]any, dir string) {
	return func(cfg map[string]any, dir string) {
		withAnthropicProvider(upstreamURL)(cfg, dir)
		cfg["pricing"] = map[string]any{
			"overrides": map[string]any{
				"up": map[string]any{
					"claude-test": map[string]any{"input_per_mtok": 1000000.0, "output_per_mtok": 1000000.0},
				},
			},
		}
	}
}

func putTeam(t *testing.T, adminURL, name, jsonBody string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, adminURL+"/admin/teams/"+name, strings.NewReader(jsonBody))
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /admin/teams/%s: %v", name, err)
	}
	return resp
}

func deleteTeam(t *testing.T, adminURL, name string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, adminURL+"/admin/teams/"+name, nil)
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /admin/teams/%s: %v", name, err)
	}
	return resp
}

func TestE2ECapabilitiesReportsTeamsRecords(t *testing.T) {
	up := newAnthropicUpstream(t)
	_, adminURL, _ := bootGateway(t, withAnthropicProvider(up.srv.URL))

	req, _ := http.NewRequest(http.MethodGet, adminURL+"/admin/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var caps struct {
		TeamsRecords bool `json:"teams_records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if !caps.TeamsRecords {
		t.Fatal("capabilities.teams_records = false, want true (the keystore always supports TeamStore, D3)")
	}
}

// TestE2ETeamRecordEnforcesDynamicallyNoRestart is the core D3 claim end to
// end: a team with no record and no config entry is ungoverned; PUT-ing a
// budget record makes it enforce on a LATER request in the SAME running
// process (no restart, no SIGHUP); DELETE-ing the record reverts it to
// ungoverned again.
func TestE2ETeamRecordEnforcesDynamicallyNoRestart(t *testing.T) {
	up := newAnthropicUpstream(t)
	dataURL, adminURL, _ := bootGateway(t, teamsAPIConfig(up.srv.URL))

	_, key := createKey(t, adminURL, "dyn-team", []string{"claude-test"})

	// No team record and no config team exist yet — ungoverned.
	r1 := postMessages(t, dataURL, key, "claude-test")
	io.Copy(io.Discard, r1.Body)
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("pre-record request: status %d, want 200 (ungoverned)", r1.StatusCode)
	}

	// A budget so tiny that a single ~15 µUSD request exhausts it.
	putResp := putTeam(t, adminURL, "dyn-team", `{"budget_usd_micros":1,"budget_on_exceeded":"block"}`)
	body, _ := io.ReadAll(putResp.Body)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /admin/teams/dyn-team: status %d: %s", putResp.StatusCode, body)
	}

	// First request under the new record: pre-check sees zero accumulated
	// spend (budget pre-check only looks at ALREADY-spent, not the incoming
	// request's own cost — governance.go's documented §5.3 behavior), so it
	// is allowed and then settles, debiting past the 1 µUSD limit.
	r2 := postMessages(t, dataURL, key, "claude-test")
	io.Copy(io.Discard, r2.Body)
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("first request under new record: status %d, want 200 (budget not yet spent)", r2.StatusCode)
	}

	// Second request: the record now enforces — no restart, no reload, the
	// SAME running gateway process now blocks. This is the D3 claim.
	r3 := postMessages(t, dataURL, key, "claude-test")
	io.Copy(io.Discard, r3.Body)
	r3.Body.Close()
	if r3.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("request after budget exhausted: status %d, want 402 (dynamic enforcement, no restart)", r3.StatusCode)
	}

	// Deleting the record reverts the team to ungoverned (no config entry for
	// "dyn-team" either) — deleting a record is not a key revoke.
	delResp := deleteTeam(t, adminURL, "dyn-team")
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /admin/teams/dyn-team: status %d, want 204", delResp.StatusCode)
	}

	r4 := postMessages(t, dataURL, key, "claude-test")
	got, _ := io.ReadAll(r4.Body)
	r4.Body.Close()
	if r4.StatusCode != http.StatusOK {
		t.Fatalf("request after record deleted: status %d: %s, want 200 (reverted to ungoverned)", r4.StatusCode, got)
	}
}

// TestE2ETeamRecordWinsOverConfig pins ADR-016's precedence rule end to end:
// a team declared in BOTH the config file and a DB record uses the RECORD's
// policy, not the config's — editing a config-declared team's budget via the
// console must not be silently shadowed by the file.
func TestE2ETeamRecordWinsOverConfig(t *testing.T) {
	up := newAnthropicUpstream(t)
	dataURL, adminURL, _ := bootGateway(t, func(cfg map[string]any, dir string) {
		teamsAPIConfig(up.srv.URL)(cfg, dir)
		cfg["teams"] = map[string]any{
			// Config says this team is effectively unlimited.
			"both": map[string]any{"budget": map[string]any{"usd_per_month": 1_000_000.0, "on_exceeded": "block"}},
		}
	})

	_, key := createKey(t, adminURL, "both", []string{"claude-test"})

	// A DB record with a near-zero budget must override the config's
	// effectively-unlimited one.
	putResp := putTeam(t, adminURL, "both", `{"budget_usd_micros":1,"budget_on_exceeded":"block"}`)
	putResp.Body.Close()

	r1 := postMessages(t, dataURL, key, "claude-test") // spends past the 1 µUSD record budget
	io.Copy(io.Discard, r1.Body)
	r1.Body.Close()
	r2 := postMessages(t, dataURL, key, "claude-test")
	body, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("record must win over an unlimited config policy for the same team: status %d: %s", r2.StatusCode, body)
	}
}

// TestE2ETeamGuardrailFieldsRoundTrip (D6, ADR-019): a team's guardrail
// override round-trips through PUT -> GET, and the same team record can still
// serve ordinary (non-bedrock) traffic unaffected — GuardrailID/Version are
// silently ignored by every provider except bedrock (§8 provider isolation).
func TestE2ETeamGuardrailFieldsRoundTrip(t *testing.T) {
	up := newAnthropicUpstream(t)
	dataURL, adminURL, _ := bootGateway(t, teamsAPIConfig(up.srv.URL))

	putResp := putTeam(t, adminURL, "guarded", `{"guardrail_id":"gr-abc123","guardrail_version":"3"}`)
	putBody, _ := io.ReadAll(putResp.Body)
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT team with guardrail fields: status %d: %s", putResp.StatusCode, putBody)
	}
	var putOut map[string]any
	json.Unmarshal(putBody, &putOut)
	if putOut["guardrail_id"] != "gr-abc123" || putOut["guardrail_version"] != "3" {
		t.Fatalf("PUT response missing guardrail fields: %+v", putOut)
	}

	req, _ := http.NewRequest(http.MethodGet, adminURL+"/admin/teams", nil)
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	getResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/teams: %v", err)
	}
	getBody, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	var list struct {
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(getBody, &list)
	if len(list.Data) != 1 || list.Data[0]["guardrail_id"] != "gr-abc123" || list.Data[0]["guardrail_version"] != "3" {
		t.Fatalf("GET /admin/teams did not reflect guardrail fields: %+v", list.Data)
	}

	// The team's ordinary (anthropic, non-bedrock) traffic is unaffected —
	// GuardrailID/Version reach the ProxyRequest but every non-bedrock
	// provider ignores them.
	_, key := createKey(t, adminURL, "guarded", []string{"claude-test"})
	resp := postMessages(t, dataURL, key, "claude-test")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request for a guardrail-configured team: status %d: %s", resp.StatusCode, body)
	}
}
