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
	Session                string            `json:"session"`
	Database               string            `json:"database"`
	Paused                 bool              `json:"paused"`
	Through                int64             `json:"through"`
	PendingSources         int64             `json:"pending_sources"`
	PendingChars           int64             `json:"pending_chars"`
	EstimatedPendingTokens int64             `json:"estimated_pending_tokens"`
	LastCheckpointAt       string            `json:"last_checkpoint_at,omitempty"`
	ImportedFrom           *SessionReference `json:"imported_from,omitempty"`
	Active                 Counts            `json:"active"`
}
type Recall struct {
	Entry        *Entry   `json:"entry,omitempty"`
	Observations []Entry  `json:"observations,omitempty"`
	Sources      []Source `json:"sources"`
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
