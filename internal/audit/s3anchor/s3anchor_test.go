package s3anchor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/inferplane/inferplane/internal/audit"
)

// stubObject is one fake stored object, keyed and dated the way S3 reports
// them back — Latest sorts on LastModified, not on the key string.
type stubObject struct {
	key  string
	body []byte
	mod  time.Time
}

type stubS3 struct {
	last    *s3.PutObjectInput
	objects []stubObject
	// pageSize, when >0, forces ListObjectsV2 to paginate — so a test can
	// prove Latest walks every page rather than trusting the first.
	pageSize int
	listErr  error
	getErr   error
}

func (s *stubS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	s.last = in
	return &s3.PutObjectOutput{}, nil
}

func (s *stubS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	prefix := ""
	if in.Prefix != nil {
		prefix = *in.Prefix
	}
	var matched []stubObject
	for _, o := range s.objects {
		if strings.HasPrefix(o.key, prefix) {
			matched = append(matched, o)
		}
	}
	start := 0
	if in.ContinuationToken != nil {
		fmt.Sscanf(*in.ContinuationToken, "%d", &start)
	}
	end := len(matched)
	truncated := false
	if s.pageSize > 0 && start+s.pageSize < end {
		end = start + s.pageSize
		truncated = true
	}
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(truncated)}
	for _, o := range matched[start:end] {
		mod := o.mod
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(o.key), LastModified: &mod})
	}
	if truncated {
		out.NextContinuationToken = aws.String(fmt.Sprintf("%d", end))
	}
	return out, nil
}

func (s *stubS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	for _, o := range s.objects {
		if in.Key != nil && o.key == *in.Key {
			return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(o.body))}, nil
		}
	}
	return nil, fmt.Errorf("stub: no such key %q", aws.ToString(in.Key))
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

func mustJSON(t *testing.T, p audit.AnchorPoint) []byte {
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLatestReturnsNilWhenInstanceHasNoAnchors(t *testing.T) {
	stub := &stubS3{}
	a := newWithClient(stub, "b", "anchors", 0)
	got, err := a.Latest(context.Background(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil (no anchors yet is not tamper evidence)", got)
	}
}

func TestLatestPicksMostRecentByLastModifiedNotKeyOrder(t *testing.T) {
	old := audit.AnchorPoint{Instance: "inst-1", HeadHash: "sha256:old", Count: 10, TS: "2026-06-14T00:00:00Z"}
	newer := audit.AnchorPoint{Instance: "inst-1", HeadHash: "sha256:new", Count: 50, TS: "2026-06-14T01:00:00Z"}
	stub := &stubS3{objects: []stubObject{
		// Deliberately store the NEWER one under a key that sorts BEFORE the
		// older one, so a key-string-sort implementation would pick wrong.
		{key: "anchors/inst-1/a-newer.json", body: mustJSON(t, newer), mod: time.Date(2026, 6, 14, 1, 0, 0, 0, time.UTC)},
		{key: "anchors/inst-1/z-older.json", body: mustJSON(t, old), mod: time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)},
	}}
	a := newWithClient(stub, "b", "anchors", 0)
	got, err := a.Latest(context.Background(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != newer {
		t.Fatalf("got %+v, want %+v", got, newer)
	}
}

func TestLatestScopesToInstancePrefix(t *testing.T) {
	mine := audit.AnchorPoint{Instance: "inst-1", HeadHash: "sha256:mine", Count: 5, TS: "2026-06-14T00:00:00Z"}
	other := audit.AnchorPoint{Instance: "inst-2", HeadHash: "sha256:other", Count: 999, TS: "2026-06-14T02:00:00Z"}
	stub := &stubS3{objects: []stubObject{
		{key: "anchors/inst-1/x.json", body: mustJSON(t, mine), mod: time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)},
		{key: "anchors/inst-2/y.json", body: mustJSON(t, other), mod: time.Date(2026, 6, 14, 2, 0, 0, 0, time.UTC)},
	}}
	a := newWithClient(stub, "b", "anchors", 0)
	got, err := a.Latest(context.Background(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != mine {
		t.Fatalf("got %+v, want the inst-1 anchor %+v (not inst-2's, despite its later timestamp)", got, mine)
	}
}

func TestLatestWalksEveryPage(t *testing.T) {
	// 5 objects, page size 2: three ListObjectsV2 pages. The true latest must
	// be found even though it is not on the first page.
	var objs []stubObject
	for i := range 5 {
		p := audit.AnchorPoint{Instance: "inst-1", HeadHash: fmt.Sprintf("sha256:%d", i), Count: int64(i), TS: "t"}
		objs = append(objs, stubObject{
			key:  fmt.Sprintf("anchors/inst-1/%d.json", i),
			body: mustJSON(t, p),
			mod:  time.Date(2026, 6, 14, 0, 0, i, 0, time.UTC), // increasing: index 4 is latest
		})
	}
	stub := &stubS3{objects: objs, pageSize: 2}
	a := newWithClient(stub, "b", "anchors", 0)
	got, err := a.Latest(context.Background(), "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	want := audit.AnchorPoint{Instance: "inst-1", HeadHash: "sha256:4", Count: 4, TS: "t"}
	if got == nil || *got != want {
		t.Fatalf("got %+v, want %+v (must walk past the first page)", got, want)
	}
}

func TestLatestPropagatesListError(t *testing.T) {
	stub := &stubS3{listErr: errors.New("boom")}
	a := newWithClient(stub, "b", "anchors", 0)
	if _, err := a.Latest(context.Background(), "inst-1"); err == nil {
		t.Fatal("want an error, got nil")
	}
}

var _ audit.AnchorReader = (*Anchorer)(nil)
