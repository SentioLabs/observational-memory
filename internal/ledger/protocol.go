package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// DecodeCheckpointV2 validates the v2 checkpoint envelope before callers use
// the value. Required numeric fields are presence-checked so zero remains a
// valid initial cursor or revision.
func DecodeCheckpointV2(data []byte) (CheckpointV2, error) {
	var value CheckpointV2
	if err := Decode(data, &value); err != nil {
		return value, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return value, err
	}
	for _, key := range []string{"expected_through", "expected_revision", "acknowledge"} {
		raw, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return value, fmt.Errorf("%s is required", key)
		}
	}
	return value, nil
}

// EncodeResponse creates one complete model-facing response under the shared
// protocol envelope, including its final newline.
func EncodeResponse(value any) ([]byte, error) {
	var body []byte
	var err error
	if text, ok := value.(string); ok {
		body = []byte(text)
	} else {
		body, err = JSON(value)
	}
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("response must be valid UTF-8")
	}
	body = append(body, '\n')
	if len(body) > ReadEnvelopeBytes {
		return nil, fmt.Errorf("response exceeds %d-byte envelope", ReadEnvelopeBytes)
	}
	return body, nil
}

func RequireProtocol(version int) (bool, error) {
	if version != ProtocolVersionV2 {
		return false, fmt.Errorf("unsupported protocol %d", version)
	}
	return true, nil
}
