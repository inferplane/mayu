package main

import (
	"strings"
	"testing"
)

// fixtureLines builds a JSONL audit log; each line is a compact record.
func reportFixture() string {
	return strings.Join([]string{
		// settled completions: team "alpha" two models, team with a comma+quote.
		`{"schema_version":1,"event":"request_completed","id":"1","ts":"2026-06-10T00:00:00Z","principal":{"key_id":"ik_a","team":"alpha"},"request":{"ingress":"anthropic","model_requested":"claude-sonnet-4-6","model_resolved":"global.anthropic.claude-sonnet-4-6"},"cost":{"amount_usd_micros":1500}}`,
		`{"schema_version":1,"event":"request_completed","id":"2","ts":"2026-06-11T00:00:00Z","principal":{"key_id":"ik_a","team":"alpha"},"request":{"ingress":"anthropic","model_requested":"claude-sonnet-4-6","model_resolved":"global.anthropic.claude-sonnet-4-6"},"cost":{"amount_usd_micros":2500}}`,
		`{"schema_version":1,"event":"request_completed","id":"3","ts":"2026-06-11T00:00:00Z","principal":{"key_id":"ik_a","team":"alpha"},"request":{"ingress":"anthropic","model_requested":"haiku","model_resolved":"haiku-up"},"cost":{"amount_usd_micros":1000000}}`,
		`{"schema_version":1,"event":"request_completed","id":"4","ts":"2026-06-11T00:00:00Z","principal":{"key_id":"ik_b","team":"weird,\"team"},"request":{"ingress":"openai","model_requested":"m","model_resolved":"m-up"},"cost":{"amount_usd_micros":42}}`,
		// excluded: started (no cost), denial, completed with nil cost.
		`{"schema_version":1,"event":"request_started","id":"5","ts":"2026-06-11T00:00:00Z","principal":{"team":"alpha"},"request":{"ingress":"anthropic","model_requested":"x"}}`,
		`{"schema_version":1,"event":"admin_denied","id":"6","ts":"2026-06-11T00:00:00Z","principal":{"team":"alpha"}}`,
		`{"schema_version":1,"event":"request_completed","id":"7","ts":"2026-06-11T00:00:00Z","principal":{"team":"alpha"},"request":{"ingress":"anthropic"}}`,
	}, "\n")
}

func TestRunReportByTeam(t *testing.T) {
	out, skipped, err := runReport(strings.NewReader(reportFixture()), reportOpts{by: "team"})
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("skipped=%d, want 0 (all lines well-formed)", skipped)
	}
	s := mustCSV(t, out)
	// alpha = 1500+2500+1000000 = 1004000 µUSD → $1.004000
	if !strings.Contains(s, "alpha,1004000,$1.004000") {
		t.Fatalf("alpha total wrong:\n%s", s)
	}
	// the comma/quote team must be CSV-escaped (encoding/csv quotes it).
	if !strings.Contains(s, `"weird,""team",42,$0.000042`) {
		t.Fatalf("CSV escaping wrong:\n%s", s)
	}
}

func TestRunReportByTeamModelUsesResolved(t *testing.T) {
	out, _, err := runReport(strings.NewReader(reportFixture()), reportOpts{by: "team,model"})
	if err != nil {
		t.Fatal(err)
	}
	s := mustCSV(t, out)
	// grouped by RESOLVED model id, not requested.
	if !strings.Contains(s, "alpha,global.anthropic.claude-sonnet-4-6,4000,") {
		t.Fatalf("resolved-model grouping wrong:\n%s", s)
	}
	if !strings.Contains(s, "alpha,haiku-up,1000000,") {
		t.Fatalf("expected resolved haiku-up row:\n%s", s)
	}
	if strings.Contains(s, "claude-sonnet-4-6,") && !strings.Contains(s, "global.anthropic") {
		t.Fatalf("must not group by requested model:\n%s", s)
	}
}

func TestRunReportTimeFilter(t *testing.T) {
	// since inclusive, until exclusive: keep only ts == 2026-06-11 records.
	out, _, err := runReport(strings.NewReader(reportFixture()), reportOpts{
		by: "team", since: "2026-06-11T00:00:00Z", until: "2026-06-12T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := mustCSV(t, out)
	// alpha on 06-11 = 2500 + 1000000 = 1002500 (the 1500 on 06-10 excluded).
	if !strings.Contains(s, "alpha,1002500,") {
		t.Fatalf("time filter wrong:\n%s", s)
	}
}

func TestRunReportEdgeCases(t *testing.T) {
	// empty input → no error, no skips, header-only CSV (non-empty).
	out, sk, err := runReport(strings.NewReader(""), reportOpts{by: "team"})
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if sk != 0 {
		t.Fatalf("empty: skipped=%d, want 0", sk)
	}
	if !strings.Contains(mustCSV(t, out), "team,micro_usd,usd") {
		t.Fatalf("empty: want header row, got %q", mustCSV(t, out))
	}
	// all-unsettled
	unsettled := `{"schema_version":1,"event":"request_started","id":"1","ts":"2026-06-11T00:00:00Z","principal":{"team":"a"}}`
	if _, _, err := runReport(strings.NewReader(unsettled), reportOpts{by: "team"}); err != nil {
		t.Fatalf("all-unsettled: %v", err)
	}
	// malformed JSON line is skipped + counted, not fatal.
	mixed := `{"event":"request_completed","ts":"2026-06-11T00:00:00Z","principal":{"team":"a"},"cost":{"amount_usd_micros":10}}` + "\n" + `{ not json` + "\n"
	_, skipped, err2 := runReport(strings.NewReader(mixed), reportOpts{by: "team"})
	err = err2
	if err != nil || skipped != 1 {
		t.Fatalf("malformed: err=%v skipped=%d (want 1)", err, skipped)
	}
	// partial trailing line (no newline) is trimmed, not parsed as a record.
	partial := `{"event":"request_completed","ts":"2026-06-11T00:00:00Z","principal":{"team":"a"},"cost":{"amount_usd_micros":10}}` + "\n" + `{"event":"request_comp`
	_, skipped, err = runReport(strings.NewReader(partial), reportOpts{by: "team"})
	if err != nil || skipped != 0 {
		t.Fatalf("partial tail: err=%v skipped=%d (want 0 — tail trimmed, not skipped)", err, skipped)
	}
}

func TestFormatUSDFromMicros(t *testing.T) {
	cases := map[int64]string{
		0:                          "$0.000000",
		42:                         "$0.000042",
		1_000_000:                  "$1.000000",
		1_004_000:                  "$1.004000",
		-2_500_000:                 "-$2.500000",
		9_223_372_036_854_775_807:  "$9223372036854.775807",  // max int64
		-9_223_372_036_854_775_808: "-$9223372036854.775808", // min int64 — no overflow
	}
	for micros, want := range cases {
		if got := formatUSDFromMicros(micros); got != want {
			t.Fatalf("formatUSDFromMicros(%d) = %q, want %q", micros, got, want)
		}
	}
}

func mustCSV(t *testing.T, b []byte) string {
	t.Helper()
	return string(b)
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errStubRead }

var errStubRead = &reportReadErr{}

type reportReadErr struct{}

func (*reportReadErr) Error() string { return "boom" }

func TestRunReportPropagatesReadError(t *testing.T) {
	if _, _, err := runReport(errReader{}, reportOpts{by: "team"}); err == nil {
		t.Fatal("a read error must propagate, not become an empty report")
	}
}
