// Package ledger stores source evidence and validated memory independently of agent clients.
package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const SourceLimit = 24000
const PendingLimit = 48000

// ViewLimit is measured in UTF-8 bytes so host transport limits remain predictable.
const ViewLimit = 12000

const (
	ProtocolVersionV2      = 2
	LedgerSchemaV2         = 2
	ReadEnvelopeBytes      = 12000
	EvidenceTextJSONBytes  = 2048
	WorkingStateBytes      = 3500
	MaxCheckpointItems     = 64
	MaxPendingPageItems    = 24
	MaxDeferralReasonBytes = 256
)

type ReviewState string

const (
	ReviewPending  ReviewState = "pending"
	ReviewReviewed ReviewState = "reviewed"
	ReviewDeferred ReviewState = "deferred"
)

type CoverageCount struct {
	Units int64 `json:"units"`
	Bytes int64 `json:"bytes"`
}

type Coverage struct {
	Pending  CoverageCount `json:"pending"`
	Reviewed CoverageCount `json:"reviewed"`
	Deferred CoverageCount `json:"deferred"`
}

type SourceDeferral struct {
	SourceID string `json:"source_id"`
	Reason   string `json:"reason"`
}

type EvidenceUnit struct {
	Seq              int64       `json:"seq"`
	ID               string      `json:"id"`
	SourceID         string      `json:"source_id"`
	Kind             string      `json:"kind"`
	Timestamp        string      `json:"timestamp"`
	StartByte        int64       `json:"start_byte"`
	EndByte          int64       `json:"end_byte"`
	Text             string      `json:"text"`
	SourceIncomplete bool        `json:"source_incomplete"`
	ReviewState      ReviewState `json:"review_state"`
	DeferralReason   string      `json:"deferral_reason,omitempty"`
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type PendingPage struct {
	Through  int64              `json:"through"`
	Revision int64              `json:"revision"`
	Coverage Coverage           `json:"coverage"`
	Page     Page[EvidenceUnit] `json:"page"`
}

type WorkingFact struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type WorkingState struct {
	Objective   *WorkingFact  `json:"objective"`
	Constraints []WorkingFact `json:"constraints"`
	Completed   []WorkingFact `json:"completed"`
	Open        []WorkingFact `json:"open"`
	Next        []WorkingFact `json:"next"`
}

type ObservationV2 struct {
	Text        string   `json:"text"`
	Importance  string   `json:"importance,omitempty"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type CheckpointV2 struct {
	ExpectedThrough  int64            `json:"expected_through"`
	ExpectedRevision int64            `json:"expected_revision"`
	Acknowledge      []string         `json:"acknowledge"`
	DeferSources     []SourceDeferral `json:"defer_sources,omitempty"`
	ReviewDeferred   []string         `json:"review_deferred,omitempty"`
	Observations     []ObservationV2  `json:"observations,omitempty"`
	Reflections      []Reflection     `json:"reflections,omitempty"`
	Retire           []Retirement     `json:"retire,omitempty"`
	WorkingState     *WorkingState    `json:"working_state,omitempty"`
}

type ReceiptV2 struct {
	Through      int64    `json:"through"`
	Revision     int64    `json:"revision"`
	Observations []string `json:"observations"`
	Reflections  []string `json:"reflections"`
	Retired      []string `json:"retired"`
	Coverage     Coverage `json:"coverage"`
}

type Source struct {
	Seq       int64  `json:"seq"`
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Timestamp string `json:"timestamp"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}
type Entry struct {
	Seq        int64       `json:"seq"`
	ID         string      `json:"id"`
	Kind       string      `json:"kind"`
	Timestamp  string      `json:"timestamp"`
	Importance string      `json:"importance"`
	Text       string      `json:"text"`
	Support    []string    `json:"support"`
	Active     bool        `json:"active"`
	Retirement *Retirement `json:"retirement"`
}
type Retirement struct {
	ID             string   `json:"id"`
	Reason         string   `json:"reason"`
	ReplacementIDs []string `json:"replacement_ids"`
}
type Observation struct {
	Text       string   `json:"text"`
	Importance string   `json:"importance,omitempty"`
	SourceIDs  []string `json:"source_ids"`
}
type Reflection struct {
	Text           string   `json:"text"`
	ObservationIDs []string `json:"observation_ids"`
}
type Checkpoint struct {
	Through      *int64        `json:"through"`
	Observations []Observation `json:"observations,omitempty"`
	Reflections  []Reflection  `json:"reflections,omitempty"`
	Retire       []Retirement  `json:"retire,omitempty"`
}
type Pending struct {
	Through int64    `json:"through"`
	Sources []Source `json:"sources"`
}
type Receipt struct {
	Through      int64    `json:"through"`
	Observations []string `json:"observations"`
	Reflections  []string `json:"reflections"`
	Retired      []string `json:"retired"`
}
type Counts struct {
	Observations int64 `json:"observations"`
	Reflections  int64 `json:"reflections"`
}
type SessionReference struct {
	Store   string `json:"store"`
	Session string `json:"session"`
}
type Status struct {
	Session                string                `json:"session"`
	Database               string                `json:"database"`
	Paused                 bool                  `json:"paused"`
	Through                int64                 `json:"through"`
	PendingSources         int64                 `json:"pending_sources"`
	PendingChars           int64                 `json:"pending_chars"`
	EstimatedPendingTokens int64                 `json:"estimated_pending_tokens"`
	LastCheckpointAt       string                `json:"last_checkpoint_at,omitempty"`
	ImportedFrom           *SessionReference     `json:"imported_from,omitempty"`
	Active                 Counts                `json:"active"`
	Revision               int64                 `json:"revision"`
	Coverage               Coverage              `json:"coverage"`
	StoredSourceBytes      int64                 `json:"stored_source_bytes"`
	SourceCount            int64                 `json:"source_count"`
	UnitCount              int64                 `json:"unit_count"`
	OldestPendingAgeTurns  int64                 `json:"oldest_pending_age_turns"`
	WorkingState           *WorkingStateMetadata `json:"working_state,omitempty"`
}
type Recall struct {
	Entry        *Entry   `json:"entry,omitempty"`
	Observations []Entry  `json:"observations,omitempty"`
	Sources      []Source `json:"sources"`
}

// Additional planning contracts: establish alongside the approved wire types.
type CaptureInput struct {
	Kind                    string `json:"kind"`
	Text                    string `json:"text"`
	Key                     string `json:"key"`
	RootTurnID              string `json:"root_turn_id,omitempty"`
	OriginCompletionOrdinal *int64 `json:"origin_completion_ordinal,omitempty"`
	SourceIncomplete        bool   `json:"source_incomplete,omitempty"`
}

type CaptureReceipt struct {
	SourceID    string `json:"source_id"`
	UnitCount   int64  `json:"unit_count"`
	FirstUnitID string `json:"first_unit_id"`
}

type RecordHeader struct {
	ID                  string `json:"id"`
	Kind                string `json:"kind"`
	Seq                 int64  `json:"seq"`
	Timestamp           string `json:"timestamp"`
	Active              bool   `json:"active"`
	Importance          string `json:"importance"`
	EffectiveImportance string `json:"effective_importance"`
}

type FieldFragment struct {
	RecordID   string `json:"record_id"`
	Field      string `json:"field"`
	Text       string `json:"text"`
	StartByte  int64  `json:"start_byte"`
	EndByte    int64  `json:"end_byte"`
	TotalBytes int64  `json:"total_bytes"`
}

type SupportReference struct {
	RecordID string `json:"record_id"`
	Relation string `json:"relation"`
	TargetID string `json:"target_id"`
	Ordinal  int64  `json:"ordinal"`
}

// Exactly one payload is non-nil. Headers never embed record text/support.
type InspectionItem struct {
	Header   *RecordHeader     `json:"header,omitempty"`
	Field    *FieldFragment    `json:"field,omitempty"`
	Support  *SupportReference `json:"support,omitempty"`
	Evidence *EvidenceUnit     `json:"evidence,omitempty"`
}

type InspectionPage struct {
	Page Page[InspectionItem] `json:"page"`
}

type RecallPage struct {
	ID   string               `json:"id"`
	Kind string               `json:"kind"`
	Page Page[InspectionItem] `json:"page"`
}

type SearchOptions struct {
	Cursor         string
	IncludeRetired bool
	Sources        bool
}

type SearchHit struct {
	ID             string        `json:"id"`
	Kind           string        `json:"kind"`
	Active         bool          `json:"active"`
	ReplacementIDs []string      `json:"replacement_ids,omitempty"`
	Snippet        string        `json:"snippet"`
	SourceID       string        `json:"source_id,omitempty"`
	StartByte      int64         `json:"start_byte,omitempty"`
	EndByte        int64         `json:"end_byte,omitempty"`
	Evidence       *EvidenceUnit `json:"evidence,omitempty"`
}

type SearchPage struct {
	Page Page[SearchHit] `json:"page"`
}

type PendingDebt struct {
	ThroughSeq int64
	Bytes      int64
}

type PendingDebtStatus struct {
	Bytes          int64
	OldestAgeTurns int64
}

type WorkingStateMetadata struct {
	Revision     int64  `json:"revision"`
	UpdatedAt    string `json:"updated_at"`
	AgeRootTurns int64  `json:"age_root_turns"`
}

// The interface is a contract, not a stub Ledger implementation. Add its
// concrete compile-time assertion only once T1-T6 implement every method.
type ContinuityLedger interface {
	CaptureV2(CaptureInput) (CaptureReceipt, error)
	ApplyV2(CheckpointV2) (ReceiptV2, error)
	ReadPending(string) (PendingPage, error)
	ReadEntries(string) (InspectionPage, error)
	ReadRecall(string, string) (RecallPage, error)
	ReadSearch(string, SearchOptions) (SearchPage, error)
	Status() (Status, error)
	CompleteRootTurn(string) (int64, error)
	CapturePendingDebt() (PendingDebt, error)
	PendingDebtAfterRoot(PendingDebt) (PendingDebtStatus, error)
	ClaimStopContinuation(string, string) (bool, error)
	Prime() (string, error)
	View() (string, error)
	Fork(string) (Status, error)
	Import(string, string) (Status, error)
}

func Decode(data []byte, target any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("input must be valid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}

// JSON preserves the original Rust ledger's hash encoding, including literal
// HTML characters and U+2028/U+2029. Escaped backslashes must stay escaped.
func JSON(value any) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	raw := bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
	var out bytes.Buffer
	for i := 0; i < len(raw); {
		if raw[i] == '\\' && i+1 < len(raw) {
			if i+6 <= len(raw) && (string(raw[i:i+6]) == `\u2028` || string(raw[i:i+6]) == `\u2029`) {
				if raw[i+5] == '8' {
					out.WriteRune('\u2028')
				} else {
					out.WriteRune('\u2029')
				}
				i += 6
				continue
			}
			out.Write(raw[i : i+2])
			i += 2
			continue
		}
		out.WriteByte(raw[i])
		i++
	}
	return out.Bytes(), nil
}
func identity(prefix string, value any) (string, error) {
	data, err := JSON(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%s%x", prefix, hash[:12]), nil
}
func cleanText(text string, limit int) (string, error) {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > limit {
		return "", fmt.Errorf("text must contain 1..=%d characters", limit)
	}
	return strings.TrimSpace(text), nil
}

var secrets = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`),
	regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,})\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`),
}

func Redact(text string) string {
	for _, pattern := range secrets {
		text = pattern.ReplaceAllString(text, "[REDACTED CREDENTIAL]")
	}
	return text
}
