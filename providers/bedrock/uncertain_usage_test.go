package bedrock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/inferplane/inferplane/providers"
)

func TestSDKMissingUsageAndUnknownTTLRemainUncertain(t *testing.T) {
	for _, u := range []*brtypes.TokenUsage{
		nil, {},
		{InputTokens: aws.Int32(1), OutputTokens: aws.Int32(2),
			CacheDetails: []brtypes.CacheDetail{{Ttl: "future-ttl", InputTokens: aws.Int32(10)}}},
		{InputTokens: aws.Int32(1), OutputTokens: aws.Int32(2), CacheWriteInputTokens: aws.Int32(100),
			CacheDetails: []brtypes.CacheDetail{{Ttl: brtypes.CacheTTLFiveMinutes, InputTokens: aws.Int32(40)},
				{Ttl: brtypes.CacheTTLOneHour, InputTokens: aws.Int32(20)}}},
	} {
		if !uncertainSDKUsage(u) {
			t.Fatal("incomplete SDK usage became precise")
		}
	}
	if uncertainSDKUsage(&brtypes.TokenUsage{InputTokens: aws.Int32(0), OutputTokens: aws.Int32(0)}) {
		t.Fatal("explicit zero usage is known")
	}
}

func TestConverseAdaptersPreserveAccountingUncertainty(t *testing.T) {
	fc := &fakeConverser{resp: ConverseResponse{UsageUncertain: true}}
	p := &provider{conv: fc, modelAPI: map[string]string{"m": "converse"}}
	req := &providers.ProxyRequest{Model: "m", Upstream: "m", RawBody: []byte(`{"messages":[]}`)}
	resp, err := p.Complete(context.Background(), req)
	if err != nil || resp.Parsed == nil || !resp.Parsed.Usage.AccountingUncertain {
		t.Fatalf("missing complete usage converted to exact zero: %v", err)
	}
	for _, events := range [][]ConverseStreamEvent{
		{{Kind: eventMessageStop, StopReason: "end_turn"}},
		{{Kind: eventMessageStop, StopReason: "end_turn"}, {Kind: eventUsage, UsageUncertain: true}},
	} {
		fc.streamEv = events
		seq, err := p.Stream(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		saw := false
		for ev, err := range seq {
			if err != nil {
				t.Fatal(err)
			}
			if ev.Chunk != nil && ev.Chunk.Usage != nil {
				saw = true
				if !ev.Chunk.Usage.AccountingUncertain {
					t.Fatal("stream adapter invented exact zero usage")
				}
			}
		}
		if !saw {
			t.Fatal("missing terminal observation")
		}
	}
}
