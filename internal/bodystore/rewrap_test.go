package bodystore

import (
	"bytes"
	"errors"
	"testing"
)

// TestRewrapKey_RoundTrip mirrors TestOpenEnvelope_WrongMasterKeyFailsClosed's
// two-master-key shape: a rewrapped wrapped-key opens under the NEW master key
// and recovers the same underlying data key, while the OLD master key can no
// longer open it (ADR-018 deferred key-rotation item).
func TestRewrapKey_RoundTrip(t *testing.T) {
	oldMaster, newMaster := testKey(t, 1), testKey(t, 2)
	req, resp := []byte("request body"), []byte("response body")
	env, err := sealEnvelope(oldMaster, req, resp)
	if err != nil {
		t.Fatal(err)
	}

	newNonce, newCT, err := RewrapKey(oldMaster, newMaster, env.wrappedKey.nonce, env.wrappedKey.ct)
	if err != nil {
		t.Fatalf("RewrapKey: %v", err)
	}
	env.wrappedKey = sealed{nonce: newNonce, ct: newCT}

	gotReq, gotResp, err := openEnvelope(newMaster, env)
	if err != nil {
		t.Fatalf("openEnvelope with new master key must succeed after rewrap: %v", err)
	}
	if !bytes.Equal(gotReq, req) || !bytes.Equal(gotResp, resp) {
		t.Fatalf("rewrap changed the underlying data: req=%q resp=%q", gotReq, gotResp)
	}
	if _, _, err := openEnvelope(oldMaster, env); err == nil {
		t.Fatal("the old master key must no longer open the rewrapped envelope")
	}
}

func TestRewrapKey_WrongOldKeyFails(t *testing.T) {
	realOld, wrongOld, newMaster := testKey(t, 1), testKey(t, 3), testKey(t, 2)
	env, err := sealEnvelope(realOld, []byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RewrapKey(wrongOld, newMaster, env.wrappedKey.nonce, env.wrappedKey.ct); !errors.Is(err, ErrRewrapFailed) {
		t.Fatalf("RewrapKey with the wrong old key = %v, want ErrRewrapFailed", err)
	}
}

// TestRewrapKey_MalformedDataKeyLengthFails pins the plan-gate round-1 CRITICAL
// fix: openEnvelope rejects a recovered data key whose length isn't exactly 32
// bytes (crypto.go), but the underlying open() does not check length itself —
// RewrapKey must replicate that check, or a malformed-but-authentically-sealed
// wrapped key would be silently "rewrapped" while staying broken.
func TestRewrapKey_MalformedDataKeyLengthFails(t *testing.T) {
	oldMaster, newMaster := testKey(t, 1), testKey(t, 2)
	s, err := seal(oldMaster, []byte("not-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := RewrapKey(oldMaster, newMaster, s.nonce, s.ct); !errors.Is(err, ErrRewrapFailed) {
		t.Fatalf("RewrapKey with a malformed-length data key = %v, want ErrRewrapFailed", err)
	}
}
