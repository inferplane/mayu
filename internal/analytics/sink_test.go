package analytics

import "testing"

func TestSink_ingestsCompletedIgnoresRest(t *testing.T) {
	ix, _ := OpenSQLite(":memory:")
	defer ix.Close()
	s := NewSink(ix)
	if s.Required() {
		t.Fatal("analytics sink must be best-effort (Required()==false)")
	}
	if s.Name() != "analytics" {
		t.Fatalf("name = %q, want analytics", s.Name())
	}
	if err := s.Write([]byte(`{"event":"request_completed","id":"x1","ts":"2026-06-29T00:00:00Z","principal":{"team":"t"},"request":{"model_resolved":"m"},"cost":{"amount_usd_micros":7}}`)); err != nil {
		t.Fatalf("write completed: %v", err)
	}
	if err := s.Write([]byte(`{"event":"request_started","id":"x2","ts":"2026-06-29T00:00:00Z"}`)); err != nil {
		t.Fatalf("write started: %v", err)
	}
	if err := s.Write([]byte(`not json`)); err != nil {
		t.Fatalf("malformed line must not error the chain: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, _ := ix.Summary(SummaryQuery{})
	if s2.Totals.Requests != 1 || s2.Totals.CostMicros != 7 {
		t.Fatalf("got %d/%d, want 1/7", s2.Totals.Requests, s2.Totals.CostMicros)
	}
}
