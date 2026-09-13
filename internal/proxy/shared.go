package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/inferplane/inferplane/internal/policy"
)

// SharedAuthorityClient receives policy/tier snapshots without issuing local
// grants. The assembled shared gateway reserves directly in the same authority
// namespace, proven by the binding callback on every successful sync.
type SharedAuthorityClient struct {
	instance string
	binding  func(context.Context) (string, string, error)
}

func NewSharedAuthorityClient(binding func(context.Context) (string, string, error)) (*SharedAuthorityClient, error) {
	if binding == nil {
		return nil, errors.New("shared authority binding required")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, errors.New("cannot generate shared authority instance")
	}
	return &SharedAuthorityClient{instance: hex.EncodeToString(raw[:]), binding: binding}, nil
}

func (s *SharedAuthorityClient) Request(context.Context) (policy.AuthorityRequest, error) {
	return policy.AuthorityRequest{Protocol: policy.AuthorityProtocol, Instance: s.instance}, nil
}

func (s *SharedAuthorityClient) Apply(ctx context.Context, r policy.AuthorityResponse, _ time.Duration) error {
	if err := policy.ValidateAuthorityBundle(&r); err != nil || r.AuthorityID == "" || len(r.Grants) != 0 ||
		len(r.ReportAcks) != 0 || len(r.MeterAcks) != 0 || len(r.Denied) != 0 {
		return errors.New("invalid shared authority bundle")
	}
	id, generation, err := s.binding(ctx)
	if err != nil || id != r.AuthorityID || generation != r.Generation {
		return errors.New("shared and control-plane authority bindings differ")
	}
	return nil
}

func (*SharedAuthorityClient) Wake() <-chan struct{} { return nil }
