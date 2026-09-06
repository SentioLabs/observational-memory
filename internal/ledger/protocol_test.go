package ledger

import (
	"strings"
	"testing"
	"unicode/utf8"
)

var _ int64 = EvidenceUnit{}.Seq
var _ string = EvidenceUnit{}.SourceID
var _ ReviewState = EvidenceUnit{}.ReviewState
var _ Coverage = PendingPage{}.Coverage
var _ []SourceDeferral = CheckpointV2{}.DeferSources
var _ []string = CheckpointV2{}.ReviewDeferred
var _ int64 = PendingPage{}.Revision
var _ []EvidenceUnit = Page[EvidenceUnit]{}.Items
var _ []string = CheckpointV2{}.Acknowledge
var _ *WorkingState = CheckpointV2{}.WorkingState
var _ int64 = ReceiptV2{}.Through

var _ Coverage = Status{}.Coverage
var _ int64 = CaptureReceipt{}.UnitCount
var _ *EvidenceUnit = SearchHit{}.Evidence
var _ []InspectionItem = InspectionPage{}.Page.Items

func TestCheckpointV2RequiredFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"valid zero cursors", `{"expected_through":0,"expected_revision":0,"acknowledge":[]}`, true},
		{"missing through", `{"expected_revision":0,"acknowledge":[]}`, false},
		{"null through", `{"expected_through":null,"expected_revision":0,"acknowledge":[]}`, false},
		{"missing revision", `{"expected_through":0,"acknowledge":[]}`, false},
		{"missing acknowledge", `{"expected_through":0,"expected_revision":0}`, false},
		{"unknown v1 field", `{"through":0,"expected_revision":0,"acknowledge":[]}`, false},
		{"multiple values", `{"expected_through":0,"expected_revision":0,"acknowledge":[]} {}`, false},
		{"invalid UTF-8", string([]byte{0xff}), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeCheckpointV2([]byte(tc.raw))
			if (err == nil) != tc.ok {
				t.Fatalf("DecodeCheckpointV2(%s) error = %v, want success %t", tc.raw, err, tc.ok)
			}
		})
	}
}

func TestCheckpointV2WorkingStatePresence(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantNil bool
	}{
		{"absent", `{"expected_through":0,"expected_revision":0,"acknowledge":[]}`, true},
		{"null", `{"expected_through":0,"expected_revision":0,"acknowledge":[],"working_state":null}`, true},
		{"empty object", `{"expected_through":0,"expected_revision":0,"acknowledge":[],"working_state":{}}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkpoint, err := DecodeCheckpointV2([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if (checkpoint.WorkingState == nil) != tc.wantNil {
				t.Fatalf("working state nil = %t, want %t", checkpoint.WorkingState == nil, tc.wantNil)
			}
		})
	}
}

func TestCaptureInputKeyPresenceGuidance(t *testing.T) {
	// T2's capture decoder must distinguish an explicitly supplied empty key from
	// an absent or null key. CaptureInput alone cannot preserve that distinction.
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"empty key supplied", `{"kind":"user","text":"x","key":""}`, true},
		{"missing key", `{"kind":"user","text":"x"}`, false},
		{"null key", `{"kind":"user","text":"x","key":null}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := captureInputKeyPresent([]byte(tc.raw)); got != tc.ok {
				t.Fatalf("key present = %t, want %t", got, tc.ok)
			}
		})
	}
}

func captureInputKeyPresent(data []byte) bool {
	var fields map[string]any
	if err := Decode(data, &fields); err != nil {
		return false
	}
	value, ok := fields["key"]
	return ok && value != nil
}

func TestResponseEnvelope(t *testing.T) {
	for _, n := range []int{ReadEnvelopeBytes - 1, ReadEnvelopeBytes} {
		body, err := EncodeResponse(strings.Repeat("x", n))
		if n+1 <= ReadEnvelopeBytes {
			if err != nil || len(body) != n+1 {
				t.Fatalf("EncodeResponse length = %d, error = %v", len(body), err)
			}
			if body[len(body)-1] != '\n' {
				t.Fatal("response did not end with one complete newline")
			}
		} else if err == nil || body != nil {
			t.Fatalf("oversized response body = %q, error = %v", body, err)
		}
	}

	for _, value := range []any{
		map[string]string{"control": "\u0000\n\t"},
		map[string]string{"unicode": "こんにちは🙂"},
	} {
		body, err := EncodeResponse(value)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.Valid(body) || strings.Count(string(body), "\n") != 1 || body[len(body)-1] != '\n' {
			t.Fatalf("response was not one complete UTF-8 response: %q", body)
		}
	}

	if body, err := EncodeResponse(string([]byte{0xff})); err == nil || body != nil {
		t.Fatalf("invalid UTF-8 response body = %q, error = %v", body, err)
	}
	if _, err := RequireProtocol(1); err == nil {
		t.Fatal("accepted protocol 1")
	}
	if ok, err := RequireProtocol(ProtocolVersionV2); err != nil || !ok {
		t.Fatalf("rejected protocol %d: %v", ProtocolVersionV2, err)
	}
}
