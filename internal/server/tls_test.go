package server

import "testing"

func TestValidateTLS(t *testing.T) {
	if err := ValidateTLS("", ""); err != nil {
		t.Fatalf("both empty ok: %v", err)
	}
	if err := ValidateTLS("c.pem", "k.pem"); err != nil {
		t.Fatalf("both set ok: %v", err)
	}
	if ValidateTLS("c.pem", "") == nil {
		t.Fatal("cert without key must error")
	}
	if ValidateTLS("", "k.pem") == nil {
		t.Fatal("key without cert must error")
	}
}
