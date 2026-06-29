package analytics

import (
	"encoding/json"

	"github.com/inferplane/inferplane/internal/audit"
)

type sink struct{ ix *Index }

// NewSink adapts the index to the audit.Sink fan-out. It is best-effort
// (Required()==false): a parse error or ingest error is swallowed so the
// tamper-evident chain and the data plane are never blocked by the derived
// index (§4 isolation invariant).
func NewSink(ix *Index) audit.Sink { return &sink{ix: ix} }

func (s *sink) Write(line []byte) error {
	var rec audit.Record
	if json.Unmarshal(line, &rec) != nil {
		return nil // malformed → skip, never error the chain
	}
	_ = s.ix.Ingest(rec) // best-effort; derived index must not break the writer
	return nil
}

func (s *sink) Name() string   { return "analytics" }
func (s *sink) Required() bool { return false }
func (s *sink) Close() error   { return nil } // Index lifecycle owned by the assembly
