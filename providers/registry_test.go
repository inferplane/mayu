package providers

import "testing"

func TestRegistryRegisterAndNew(t *testing.T) {
	Register("fake-m2", func(cfg Config) (Provider, error) {
		return nil, nil // factory presence is what we assert here
	})
	if _, ok := factories["fake-m2"]; !ok {
		t.Fatal("factory not registered")
	}
	if _, err := New(Config{Type: "missing-xyz"}); err == nil {
		t.Fatal("expected error for unknown provider type")
	}
}
