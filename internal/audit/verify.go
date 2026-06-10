package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
)

type VerifyResult struct {
	OK       bool
	Records  int
	BrokenAt int // 1-based index of the first record whose prev_hash mismatched (0 if OK)
	Reason   string
}

// Verify reads a JSONL audit stream and checks the hash chain: each record's
// prev_hash must equal sha256 of the PRECEDING record's exact line bytes, and
// the first record of each per-instance segment must carry the genesis hash. A
// tampered record changes its bytes, so the NEXT record's prev_hash no longer
// matches — that's the break.
func Verify(r io.Reader) (VerifyResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var n int
	expectedPrev := genesisHash
	curInstance := ""
	first := true
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		n++
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return VerifyResult{OK: false, Records: n, BrokenAt: n, Reason: "unparseable record"}, nil
		}
		// A new instance segment starts its own chain at genesis (design §5.4:
		// per-instance independent chains, identified by the instance field).
		if first || rec.Instance != curInstance {
			curInstance = rec.Instance
			expectedPrev = genesisHash
			first = false
		}
		if rec.PrevHash != expectedPrev {
			return VerifyResult{OK: false, Records: n, BrokenAt: n,
				Reason: "prev_hash mismatch — chain broken or record tampered"}, nil
		}
		sum := sha256.Sum256(line)
		expectedPrev = "sha256:" + hex.EncodeToString(sum[:])
	}
	if err := sc.Err(); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{OK: true, Records: n}, nil
}
