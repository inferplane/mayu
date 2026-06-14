package s3anchor

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/inferplane/inferplane/internal/audit"
)

type stubS3 struct{ last *s3.PutObjectInput }

func (s *stubS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.last = in
	return &s3.PutObjectOutput{}, nil
}

func TestAnchorPutsJSON(t *testing.T) {
	stub := &stubS3{}
	a := newWithClient(stub, "audit-bucket", "anchors", 0)
	p := audit.AnchorPoint{Instance: "inst-1", HeadHash: "sha256:abc", Count: 42, TS: "2026-06-14T00:00:00Z"}
	if err := a.Anchor(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	in := stub.last
	if *in.Bucket != "audit-bucket" {
		t.Fatalf("bucket = %q", *in.Bucket)
	}
	if !strings.HasPrefix(*in.Key, "anchors/inst-1/2026-06-14T00:00:00Z-42") || !strings.HasSuffix(*in.Key, ".json") {
		t.Fatalf("key = %q (want unique ts+count)", *in.Key)
	}
	body, _ := io.ReadAll(in.Body)
	var got audit.AnchorPoint
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not JSON anchor: %v", err)
	}
	if got != p {
		t.Fatalf("body = %+v want %+v", got, p)
	}
	if in.ObjectLockMode != "" {
		t.Fatalf("unexpected object-lock mode: %q", in.ObjectLockMode)
	}
}

func TestAnchorSetsRetention(t *testing.T) {
	stub := &stubS3{}
	a := newWithClient(stub, "b", "p", 7)
	a.now = func() time.Time { return time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC) }
	if err := a.Anchor(context.Background(), audit.AnchorPoint{Instance: "i", HeadHash: "h", Count: 1, TS: "2026-06-14T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	in := stub.last
	if in.ObjectLockMode != s3types.ObjectLockModeCompliance {
		t.Fatalf("retain_days>0 must set COMPLIANCE mode, got %q", in.ObjectLockMode)
	}
	want := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	if in.ObjectLockRetainUntilDate == nil || !in.ObjectLockRetainUntilDate.Equal(want) {
		t.Fatalf("retain-until = %v, want %v", in.ObjectLockRetainUntilDate, want)
	}
}

func TestAnchorBodyHasNoSecret(t *testing.T) {
	stub := &stubS3{}
	a := newWithClient(stub, "b", "p", 0)
	_ = a.Anchor(context.Background(), audit.AnchorPoint{Instance: "i", HeadHash: "h", Count: 1, TS: "t"})
	body, _ := io.ReadAll(stub.last.Body)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	allowed := map[string]bool{"instance": true, "head_hash": true, "count": true, "ts": true}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("anchor JSON leaked field %q", k)
		}
	}
}
