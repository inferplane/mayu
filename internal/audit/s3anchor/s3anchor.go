// Package s3anchor writes audit chain-head anchors to S3 (ADR-012). With the
// bucket configured for Object Lock (compliance retention), the anchors are WORM
// — an immutable external witness that upgrades the audit chain from
// tamper-evident to tamper-resistant. Uses aws-sdk-go-v2/service/s3 (sibling of
// the bedrock SDK already in the tree); pure-Go, same credential chain.
package s3anchor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/inferplane/inferplane/internal/audit"
)

// putObjectAPI is the slice of the S3 client this package uses (so a stub can
// verify the request shape offline — this environment has no S3).
type putObjectAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Config selects the bucket/prefix and optional per-object retention.
type Config struct {
	Bucket     string
	Prefix     string
	Region     string
	Endpoint   string // optional override for S3-compatible WORM (e.g. MinIO)
	RetainDays int    // >0 → set per-object COMPLIANCE retention on each anchor
}

// Anchorer puts a JSON AnchorPoint per call.
type Anchorer struct {
	client     putObjectAPI
	bucket     string
	prefix     string
	retainDays int
	now        func() time.Time // injectable for tests
}

// New builds an S3 anchorer from the default AWS credential chain.
func New(ctx context.Context, cfg Config) (*Anchorer, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awscfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3anchor: aws config: %w", err)
	}
	client := s3.NewFromConfig(awscfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true // S3-compatible stores (MinIO) need path style
		}
	})
	return newWithClient(client, cfg.Bucket, cfg.Prefix, cfg.RetainDays), nil
}

func newWithClient(client putObjectAPI, bucket, prefix string, retainDays int) *Anchorer {
	return &Anchorer{client: client, bucket: bucket, prefix: prefix, retainDays: retainDays, now: time.Now}
}

// Anchor PUTs the AnchorPoint as JSON at prefix/instance/<ts>-<count>.json (the
// nanosecond ts + count guarantee a unique key so a tick and a final anchor at
// the same second never collide). Object Lock retention is set per-object when
// configured; the bucket's Object Lock config is the operator's responsibility.
func (a *Anchorer) Anchor(ctx context.Context, p audit.AnchorPoint) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	key := path.Join(a.prefix, p.Instance, p.TS+"-"+strconv.FormatInt(p.Count, 10)+".json")
	in := &s3.PutObjectInput{
		Bucket:      aws.String(a.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	}
	if a.retainDays > 0 {
		in.ObjectLockMode = s3types.ObjectLockModeCompliance
		in.ObjectLockRetainUntilDate = aws.Time(a.now().UTC().Add(time.Duration(a.retainDays) * 24 * time.Hour))
	}
	if _, err := a.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("s3anchor: put %q: %w", key, err)
	}
	return nil
}

var _ audit.Anchorer = (*Anchorer)(nil)
