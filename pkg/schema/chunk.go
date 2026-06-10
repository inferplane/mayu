package schema

import "encoding/json"

// ChatChunk — canonical 스트리밍 이벤트. Anthropic 이벤트 어휘를 그대로
// 채택한다 (message_start/content_block_start/content_block_delta/
// content_block_stop/message_delta/message_stop/ping/error).
// delta는 M1에서 raw 보존 — SSE 직렬화기(M2)는 재방출만 하고,
// OpenAI 변환(M5)에서 타입 승격한다. usage가 실린 message_delta가
// 정산의 진실원이다 (§5.3 드레인 정산).
type ChatChunk struct {
	Type         string                     `json:"type"`
	Index        *int                       `json:"index,omitempty"`
	Message      *ChatResponse              `json:"message,omitempty"`
	ContentBlock *ContentBlock              `json:"content_block,omitempty"`
	Delta        json.RawMessage            `json:"delta,omitempty"`
	Usage        *Usage                     `json:"usage,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
}

var chatChunkKnown = []string{
	"type", "index", "message", "content_block", "delta", "usage",
}

func (c *ChatChunk) UnmarshalJSON(data []byte) error {
	type plain ChatChunk
	extra, err := unmarshalWithExtra(data, (*plain)(c), chatChunkKnown...)
	c.Extra = extra
	return err
}

func (c ChatChunk) MarshalJSON() ([]byte, error) {
	type plain ChatChunk
	return marshalWithExtra(plain(c), c.Extra)
}
