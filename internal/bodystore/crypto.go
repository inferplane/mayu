package bodystore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

var ErrRewrapFailed = errors.New("bodystore: rewrap failed")

// ParseMasterKey decodes a 64-hex-char (32-byte) AES-256 key, the shape
// audit.log_bodies.key_ref must resolve to. The error never echoes the input.
func ParseMasterKey(hexKey string) ([32]byte, error) {
	var key [32]byte
	raw, err := hex.DecodeString(hexKey)
	if err != nil || len(raw) != 32 {
		return key, errors.New("bodystore: master key must be 64 hex characters (32 bytes)")
	}
	copy(key[:], raw)
	return key, nil
}

// sealed is one AEAD-sealed value: an envelope-encrypted body or a wrapped
// per-record data key. nonce is AES-GCM's standard 12 bytes.
type sealed struct {
	nonce []byte
	ct    []byte
}

func seal(key [32]byte, plaintext []byte) (sealed, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return sealed{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return sealed{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return sealed{}, err
	}
	return sealed{nonce: nonce, ct: gcm.Seal(nil, nonce, plaintext, nil)}, nil
}

// open decrypts s with key. Any failure (wrong key, tampered ciphertext,
// malformed nonce) returns a generic error — never distinguishes WHY, so a
// caller can't be used as a decryption oracle.
func open(key [32]byte, s sealed) ([]byte, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(s.nonce) != gcm.NonceSize() {
		return nil, errors.New("bodystore: invalid nonce")
	}
	pt, err := gcm.Open(nil, s.nonce, s.ct, nil)
	if err != nil {
		return nil, fmt.Errorf("bodystore: decrypt: %w", err)
	}
	return pt, nil
}

// RewrapKey rewraps a data key from oldMaster to newMaster using only the
// wrapped-key columns; the request/response ciphertext is never touched by this
// function. It never distinguishes wrong-key from tampered-key from
// malformed-length: all unwrap problems become ErrRewrapFailed.
func RewrapKey(oldMaster, newMaster [32]byte, nonce, ct []byte) (newNonce, newCT []byte, err error) {
	dataKeyBytes, err := open(oldMaster, sealed{nonce: nonce, ct: ct})
	if err != nil || len(dataKeyBytes) != 32 {
		return nil, nil, ErrRewrapFailed
	}
	var dataKey [32]byte
	copy(dataKey[:], dataKeyBytes)
	s, err := seal(newMaster, dataKey[:])
	if err != nil {
		return nil, nil, err
	}
	return s.nonce, s.ct, nil
}

// envelope is the per-record encryption state: a fresh random data key wraps
// the actual body/response bytes; the master key wraps the data key. Rotating
// the master key means re-wrapping data keys (rewrapping every row), never
// touching the (potentially large) body ciphertext itself.
type envelope struct {
	wrappedKey sealed  // data key, sealed under the master key
	req        sealed  // request body, sealed under the data key
	resp       *sealed // response body, sealed under the data key (nil = not captured)
}

func sealEnvelope(masterKey [32]byte, req, resp []byte) (envelope, error) {
	var dataKey [32]byte
	if _, err := rand.Read(dataKey[:]); err != nil {
		return envelope{}, err
	}
	wrapped, err := seal(masterKey, dataKey[:])
	if err != nil {
		return envelope{}, err
	}
	sealedReq, err := seal(dataKey, req)
	if err != nil {
		return envelope{}, err
	}
	env := envelope{wrappedKey: wrapped, req: sealedReq}
	if resp != nil {
		sealedResp, err := seal(dataKey, resp)
		if err != nil {
			return envelope{}, err
		}
		env.resp = &sealedResp
	}
	return env, nil
}

func openEnvelope(masterKey [32]byte, env envelope) (req, resp []byte, err error) {
	dataKeyBytes, err := open(masterKey, env.wrappedKey)
	if err != nil || len(dataKeyBytes) != 32 {
		return nil, nil, errors.New("bodystore: unwrap data key failed")
	}
	var dataKey [32]byte
	copy(dataKey[:], dataKeyBytes)
	req, err = open(dataKey, env.req)
	if err != nil {
		return nil, nil, err
	}
	if env.resp != nil {
		resp, err = open(dataKey, *env.resp)
		if err != nil {
			return nil, nil, err
		}
	}
	return req, resp, nil
}
