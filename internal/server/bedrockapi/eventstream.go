package bedrockapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

func writeChunkFrame(w io.Writer, enc *eventstream.Encoder, chunkJSON []byte) error {
	payload, err := json.Marshal(struct {
		Bytes string `json:"bytes"`
	}{
		Bytes: base64.StdEncoding.EncodeToString(chunkJSON),
	})
	if err != nil {
		return fmt.Errorf("marshal chunk frame payload: %w", err)
	}
	headers := eventstream.Headers{
		{Name: ":message-type", Value: eventstream.StringValue("event")},
		{Name: ":event-type", Value: eventstream.StringValue("chunk")},
		{Name: ":content-type", Value: eventstream.StringValue("application/json")},
	}
	if err := enc.Encode(w, eventstream.Message{Headers: headers, Payload: payload}); err != nil {
		return fmt.Errorf("encode chunk frame: %w", err)
	}
	return nil
}

func writeExceptionFrame(w io.Writer, enc *eventstream.Encoder, errType, message string) error {
	payload, err := json.Marshal(struct {
		Message string `json:"message"`
	}{
		Message: message,
	})
	if err != nil {
		return fmt.Errorf("marshal exception frame payload: %w", err)
	}
	headers := eventstream.Headers{
		{Name: ":message-type", Value: eventstream.StringValue("exception")},
		{Name: ":exception-type", Value: eventstream.StringValue(errType)},
	}
	if err := enc.Encode(w, eventstream.Message{Headers: headers, Payload: payload}); err != nil {
		return fmt.Errorf("encode exception frame: %w", err)
	}
	return nil
}
