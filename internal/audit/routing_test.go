package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRoutingLegacyBytesAndMixedChain(t *testing.T) {
	const oldRequest = `{"ingress":"anthropic","model_requested":"economy","model_resolved":"up","stream":false,"model_substituted_from":"premium"}`
	var request RequestRef
	if err := json.Unmarshal([]byte(oldRequest), &request); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(request)
	if string(got) != oldRequest {
		t.Fatalf("legacy audit bytes changed: %s", got)
	}
	// Literal old record anchors a mixed old/new/old chain; remarshal must not
	// synthesize a routing member on either legacy record.
	const old = `{"schema_version":1,"event":"request_started","id":"old","ts":"2026-09-09T00:00:00Z","instance":"i","principal":{"key_id":"opaque","team":"t"},"request":{"ingress":"anthropic","model_requested":"m","stream":false},"trace_id":null,"prev_hash":"sha256:genesis"}`
	var rec Record
	if err := json.Unmarshal([]byte(old), &rec); err != nil {
		t.Fatal(err)
	}
	line, _ := rec.Canonical()
	if string(line) != old {
		t.Fatalf("old fixture changed: %s", line)
	}
	hash := func(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }
	rec.ID = "new"
	rec.PrevHash = hash([]byte(old))
	rec.Request.Routing = &RoutingRef{RequestedModel: "m", SelectedModel: "m", Reason: "internal_only", Inspection: "complete", PlannedProvider: "private", PlannedBoundary: "internal", Masked: true}
	next, _ := rec.Canonical()
	rec.ID = "old-again"
	rec.Request.Routing = nil
	rec.PrevHash = hash(next)
	last, _ := rec.Canonical()
	fixture := old + "\n" + string(next) + "\n" + string(last) + "\n"
	result, err := Verify(strings.NewReader(fixture))
	if err != nil || !result.OK || result.Records != 3 {
		t.Fatalf("mixed chain: %+v %v", result, err)
	}
	broken := strings.Replace(fixture, `"planned_boundary":"internal"`, `"planned_boundary":"external"`, 1)
	result, err = Verify(strings.NewReader(broken))
	if err != nil || result.OK || result.BrokenAt != 3 {
		t.Fatal("mixed fixture did not detect tampering")
	}
}

func TestRoutingContextCopiesEveryMutableValue(t *testing.T) {
	d := &RoutingRef{RequestedModel: "m", Reason: "internal_only", Inspection: "complete",
		Categories: []string{"email"}, Policies: []RoutingPolicyRef{{Name: "policy", Generation: 1, Rule: "rule"}},
		Recommendations: []RoutingRecommendation{{Model: "cheap"}}, PlannedProvider: "first", PlannedBoundary: "internal"}
	ctx := WithRouting(context.Background(), d)
	d.Categories[0] = "changed"
	d.Policies[0].Name = "changed"
	d.Recommendations[0].Model = "changed"
	first := WithRoutingAttempt(ctx, "m", "first", "internal")
	second := WithRoutingAttempt(ctx, "m", "second", "internal")
	f := RoutingFrom(first)
	if f.Categories[0] != "email" || f.Policies[0].Name != "policy" || f.Recommendations[0].Model != "cheap" {
		t.Fatal("context aliases caller metadata")
	}
	f.Categories[0] = "changed"
	if RoutingFrom(first).Categories[0] != "email" || RoutingFrom(first).ActualProvider != "first" || RoutingFrom(second).ActualProvider != "second" || RoutingFrom(ctx).ActualProvider != "" {
		t.Fatal("attempt or emitted record drifted")
	}
	if RoutingFrom(WithRoutingAttempt(context.Background(), "m", "p", "internal")) != nil {
		t.Fatal("no-policy traffic acquired routing evidence")
	}
}

func TestRoutingDTOContainsOnlyScalarMetadata(t *testing.T) {
	var check func(reflect.Type)
	check = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.String, reflect.Int64, reflect.Bool:
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				check(typ.Field(i).Type)
			}
		case reflect.Slice:
			check(typ.Elem())
		default:
			t.Fatalf("routing audit DTO can retain non-scalar state: %s", typ)
		}
	}
	check(reflect.TypeOf(RoutingRef{}))
}
