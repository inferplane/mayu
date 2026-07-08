package bedrock

import (
	"context"
	"iter"
)

type fakeInvoker struct {
	gotModelID   string
	gotBody      []byte
	gotGuardrail Guardrail
	respBody     []byte
	streamRaw    [][]byte
	err          error
}

func (f *fakeInvoker) Invoke(_ context.Context, modelID string, body []byte, g Guardrail) ([]byte, error) {
	f.gotModelID = modelID
	f.gotBody = append([]byte(nil), body...)
	f.gotGuardrail = g
	return f.respBody, f.err
}
func (f *fakeInvoker) InvokeStream(_ context.Context, modelID string, body []byte, g Guardrail) (iter.Seq2[[]byte, error], error) {
	f.gotModelID = modelID
	f.gotBody = append([]byte(nil), body...)
	f.gotGuardrail = g
	if f.err != nil {
		return nil, f.err
	}
	return func(yield func([]byte, error) bool) {
		for _, b := range f.streamRaw {
			if !yield(b, nil) {
				return
			}
		}
	}, nil
}

type fakeConverser struct {
	resp       ConverseResponse
	streamEv   []ConverseStreamEvent
	gotReq     ConverseRequest
	gotModelID string
}

func (f *fakeConverser) Converse(_ context.Context, modelID string, req ConverseRequest) (ConverseResponse, error) {
	f.gotModelID = modelID
	f.gotReq = req
	return f.resp, nil
}
func (f *fakeConverser) ConverseStream(_ context.Context, modelID string, req ConverseRequest) (iter.Seq2[ConverseStreamEvent, error], error) {
	f.gotModelID = modelID
	f.gotReq = req
	return func(yield func(ConverseStreamEvent, error) bool) {
		for _, e := range f.streamEv {
			if !yield(e, nil) {
				return
			}
		}
	}, nil
}
