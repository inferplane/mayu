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
	// Instances is the final {head hash, record count} per per-instance chain
	// segment seen in the stream (design §5.4: each `instance` value starts its
	// own chain at genesis). A caller holding an external anchor (ADR-012)
	// cross-checks it here — see AnchorReader and the auditapi wiring — because
	// Verify alone proves only internal consistency: a whole-file replacement
	// starting from a fresh genesis, or a truncated tail, verifies as OK with
	// no signal that anything is missing.
	Instances map[string]InstanceState
}

// InstanceState is one instance's chain position: the head hash after its
// last verified record, and how many records that segment contains.
type InstanceState struct {
	HeadHash string
	Count    int64
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
	var curCount int64
	instances := map[string]InstanceState{}
	first := true
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		n++
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return VerifyResult{OK: false, Records: n, BrokenAt: n, Reason: "unparseable record", Instances: instances}, nil
		}
		// A new instance segment starts its own chain at genesis (design §5.4:
		// per-instance independent chains, identified by the instance field).
		if first || rec.Instance != curInstance {
			curInstance = rec.Instance
			expectedPrev = genesisHash
			curCount = 0
			first = false
		}
		if rec.PrevHash != expectedPrev {
			return VerifyResult{OK: false, Records: n, BrokenAt: n,
				Reason: "prev_hash mismatch — chain broken or record tampered", Instances: instances}, nil
		}
		sum := sha256.Sum256(line)
		expectedPrev = "sha256:" + hex.EncodeToString(sum[:])
		curCount++
		instances[curInstance] = InstanceState{HeadHash: expectedPrev, Count: curCount}
	}
	if err := sc.Err(); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{OK: true, Records: n, Instances: instances}, nil
}

// HeadAtCount re-walks the same chain rules as Verify but stops as soon as
// instance's segment has accumulated exactly count records, returning the
// head hash at that exact point. It is how a caller checks a MID-chain
// anchor (one older than the file's current tail) without trusting the
// current tail's own bookkeeping — the check recomputes the chain up to that
// position independently. found reports whether the instance ever reached
// count records in this stream (false on a short/absent segment — the
// truncation signal an anchor exists to catch).
func HeadAtCount(r io.Reader, instance string, count int64) (headHash string, found bool, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	expectedPrev := genesisHash
	curInstance := ""
	var curCount int64
	first := true
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return "", false, nil // unparseable: same "not found intact" signal as a short chain
		}
		if first || rec.Instance != curInstance {
			curInstance = rec.Instance
			expectedPrev = genesisHash
			curCount = 0
			first = false
		}
		if rec.PrevHash != expectedPrev {
			return "", false, nil // broken chain before reaching count: not found intact
		}
		sum := sha256.Sum256(line)
		expectedPrev = "sha256:" + hex.EncodeToString(sum[:])
		curCount++
		if curInstance == instance && curCount == count {
			return expectedPrev, true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, err
	}
	return "", false, nil
}
