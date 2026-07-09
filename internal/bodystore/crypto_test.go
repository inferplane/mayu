package bodystore

import (
	"bytes"
	"strings"
	"testing"
)

func testKey(t *testing.T, seed byte) [32]byte {
	t.Helper()
	var k [32]byte
	for i := range k {
		k[i] = seed
	}
	return k
}

func TestParseMasterKey(t *testing.T) {
	if _, err := ParseMasterKey("not-hex"); err == nil {
		t.Fatal("expected error for non-hex input")
	}
	if _, err := ParseMasterKey("00"); err == nil {
		t.Fatal("expected error for wrong-length key")
	}
	hexKey := strings.Repeat("ab", 32)
	if _, err := ParseMasterKey(hexKey); err != nil {
		t.Fatalf("valid 64-hex-char key rejected: %v", err)
	}
}

func TestParseMasterKey_NeverEchoesInput(t *testing.T) {
	secret := "this-is-not-hex-but-looks-secret-ish"
	_, err := ParseMasterKey(secret)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error must never echo the input value: %v", err)
	}
}

func TestSealOpen_RoundTrip(t *testing.T) {
	key := testKey(t, 1)
	plaintext := []byte("the quick brown fox")
	s, err := seal(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := open(key, s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("roundtrip mismatch: got %q want %q", got, plaintext)
	}
}

func TestOpen_WrongKeyFails(t *testing.T) {
	s, err := seal(testKey(t, 1), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(testKey(t, 2), s); err == nil {
		t.Fatal("expected decryption to fail with the wrong key")
	}
}

func TestOpen_TamperedCiphertextFails(t *testing.T) {
	key := testKey(t, 1)
	s, err := seal(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	s.ct[0] ^= 0xFF
	if _, err := open(key, s); err == nil {
		t.Fatal("expected decryption to fail on tampered ciphertext")
	}
}

func TestSealOpenEnvelope_RoundTrip(t *testing.T) {
	master := testKey(t, 7)
	req, resp := []byte("request body"), []byte("response body")
	env, err := sealEnvelope(master, req, resp)
	if err != nil {
		t.Fatal(err)
	}
	gotReq, gotResp, err := openEnvelope(master, env)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotReq, req) || !bytes.Equal(gotResp, resp) {
		t.Fatalf("envelope roundtrip mismatch: req=%q resp=%q", gotReq, gotResp)
	}
}

func TestSealOpenEnvelope_NilResponseNotCaptured(t *testing.T) {
	master := testKey(t, 7)
	env, err := sealEnvelope(master, []byte("request only"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if env.resp != nil {
		t.Fatal("nil response must not produce a sealed resp envelope")
	}
	gotReq, gotResp, err := openEnvelope(master, env)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotReq) != "request only" || gotResp != nil {
		t.Fatalf("got req=%q resp=%v, want resp=nil", gotReq, gotResp)
	}
}

func TestOpenEnvelope_WrongMasterKeyFailsClosed(t *testing.T) {
	env, err := sealEnvelope(testKey(t, 7), []byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := openEnvelope(testKey(t, 8), env); err == nil {
		t.Fatal("expected unwrap failure with the wrong master key")
	}
}
