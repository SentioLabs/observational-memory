package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Positions are data, never SQL. Offset is a UTF-8 field byte offset or a
// support ordinal; Evidence is the last emitted evidence sequence.
type readPosition struct {
	Record   int64                 `json:"record"`
	Phase    string                `json:"phase"`
	Offset   int64                 `json:"offset"`
	Evidence int64                 `json:"evidence"`
	Search   *rankedSearchPosition `json:"search,omitempty"`
}
type readCursor struct {
	Version          int          `json:"version"`
	Store            string       `json:"store"`
	Session          string       `json:"session"`
	Epoch            string       `json:"epoch"`
	Command          string       `json:"command"`
	ArgumentDigest   string       `json:"argument_digest"`
	SourceHighWater  int64        `json:"source_high_water"`
	MemoryRevision   int64        `json:"memory_revision"`
	SearchGeneration int64        `json:"search_generation"`
	Position         readPosition `json:"position"`
}

func cursorDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
func encodeCursor(c readCursor) (string, error) {
	body, err := JSON(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(append(body, sum[:]...)), nil
}
func decodeCursor(token string) (readCursor, error) {
	var c readCursor
	if len(token) > 4096 {
		return c, fmt.Errorf("invalid cursor; restart pagination")
	}
	body, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(body) <= sha256.Size {
		return c, fmt.Errorf("invalid cursor; restart pagination")
	}
	data, checksum := body[:len(body)-sha256.Size], body[len(body)-sha256.Size:]
	sum := sha256.Sum256(data)
	if string(sum[:]) != string(checksum) {
		return c, fmt.Errorf("invalid cursor integrity; restart pagination")
	}
	if err = Decode(data, &c); err != nil {
		return c, fmt.Errorf("invalid cursor; restart pagination: %w", err)
	}
	// Canonical encoding also rejects duplicate or missing fields.
	canonical, err := encodeCursor(c)
	if err != nil || canonical != token {
		return c, fmt.Errorf("invalid cursor fields; restart pagination")
	}
	if c.Version != 1 || c.SourceHighWater < 0 || c.MemoryRevision < 0 || c.SearchGeneration < 0 || c.Position.Record < 0 || c.Position.Offset < 0 || c.Position.Evidence < 0 {
		return c, fmt.Errorf("unsupported cursor; restart pagination")
	}
	switch c.Position.Phase {
	case "", "header", "text", "reason", "support", "replacement", "evidence", "search":
	default:
		return c, fmt.Errorf("invalid cursor position; restart pagination")
	}
	return c, nil
}

// A DEFERRED transaction pins metadata and rows together without taking a write
// lock. Callers close rows before issuing another query on this connection.
func (l *Ledger) readSnapshot() (*sql.Conn, func(), error) {
	conn, err := l.db.Conn(l.ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err = conn.ExecContext(l.ctx, "BEGIN DEFERRED"); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK"); _ = conn.Close() }, nil
}

// Search opts into generation binding; all current reads expose review state or
// entries and bind the memory revision. Appends preserve the source high-water.
func (l *Ledger) pageCursor(q queryer, token, command, arguments string, search bool) (readCursor, error) {
	c := readCursor{Version: 1, Store: cursorDigest(l.store), Session: cursorDigest(l.session), Command: command, ArgumentDigest: cursorDigest(arguments)}
	err := q.QueryRowContext(l.ctx, `SELECT
 (SELECT value FROM meta WHERE key='pagination_epoch'),
 CAST((SELECT value FROM meta WHERE key='revision') AS INTEGER),
 CAST((SELECT value FROM meta WHERE key='search_generation') AS INTEGER),
 COALESCE((SELECT MAX(seq) FROM sources),0)`).Scan(&c.Epoch, &c.MemoryRevision, &c.SearchGeneration, &c.SourceHighWater)
	if err != nil {
		return c, err
	}
	if !search {
		c.SearchGeneration = 0
	}
	if token == "" {
		return c, nil
	}
	old, err := decodeCursor(token)
	if err != nil {
		return c, err
	}
	if old.Store != c.Store || old.Session != c.Session || old.Epoch != c.Epoch || old.Command != command || old.ArgumentDigest != c.ArgumentDigest || old.MemoryRevision != c.MemoryRevision || old.SearchGeneration != c.SearchGeneration || old.SourceHighWater > c.SourceHighWater {
		return c, fmt.Errorf("cursor scope or snapshot changed; restart pagination")
	}
	if !search && old.Position.Search != nil {
		return c, fmt.Errorf("invalid read position; restart pagination")
	}
	return old, nil
}

// packPage tests each candidate with its actual continuation token and final
// newline. next is deterministic and has no side effects, so a rejected item
// remains at the preceding cursor position for the next call.
func packPage[T any](c readCursor, cap int, next func(readPosition) (T, readPosition, bool, error), envelope func(Page[T]) any) (Page[T], error) {
	page := Page[T]{Items: []T{}}
	item, after, ok, err := next(c.Position)
	if err != nil {
		return page, err
	}
	for ok {
		_, _, more, err := next(after)
		if err != nil {
			return page, err
		}
		candidate := Page[T]{Items: append(append([]T{}, page.Items...), item)}
		if more {
			c.Position = after
			candidate.NextCursor, err = encodeCursor(c)
			if err != nil {
				return page, err
			}
		}
		if _, err = EncodeResponse(envelope(candidate)); err != nil {
			if len(page.Items) == 0 {
				return page, fmt.Errorf("first page item cannot fit: %w", err)
			}
			return page, nil
		}
		page = candidate
		if !more || len(page.Items) >= cap {
			return page, nil
		}
		item, after, ok, err = next(after)
		if err != nil {
			return page, err
		}
	}
	_, err = EncodeResponse(envelope(page))
	return page, err
}
