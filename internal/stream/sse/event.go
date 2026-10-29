// Package sse provides bounded, cancellable server-sent events over ordinary HTTP.
// It owns framing and connection lifetime; business handlers own the event payload.
package sse

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	// Sentinels expose stable categories only. They deliberately never wrap
	// network, authorization or application errors that can contain private data.
	ErrInvalidOptions = errors.New("invalid SSE options")
	ErrInvalidEvent   = errors.New("invalid SSE event")
	ErrEventTooLarge  = errors.New("SSE event exceeds limit")
	ErrUnauthorized   = errors.New("SSE authorization rejected")
	ErrUnsupported    = errors.New("SSE response writer unsupported")
	ErrWrite          = errors.New("SSE write failed")
	ErrHandler        = errors.New("SSE handler failed")
)

// Event is a single UTF-8 SSE frame. Retry is a positive whole number of milliseconds.
// Empty ID/Name/Retry omit those optional fields; Data always produces a data field.
type Event struct {
	ID    string
	Name  string
	Data  []byte
	Retry time.Duration
}

// Fields cannot contain control characters: otherwise an ID/name could inject a
// second protocol field or be silently ignored by the browser's SSE parser.
func validField(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	})
}

func encodeEvent(event Event, limit int) ([]byte, error) {
	// Reject fractional milliseconds instead of silently truncating retry values;
	// browser reconnect timing should match the value the application requested.
	if !validField(event.ID) || !validField(event.Name) || !utf8.Valid(event.Data) || event.Retry < 0 || (event.Retry > 0 && event.Retry%time.Millisecond != 0) {
		return nil, ErrInvalidEvent
	}
	// Check raw sizes before making normalized copies. The final check also counts
	// framing overhead, so many small lines cannot bypass the configured byte cap.
	if len(event.ID) > limit || len(event.Name) > limit || len(event.Data) > limit {
		return nil, ErrEventTooLarge
	}
	var out bytes.Buffer
	// Keep every field append behind the same budget check. Counting only Data
	// would leave ID, event name and per-line prefixes outside the memory limit.
	appendField := func(name, value string) error {
		if out.Len()+len(name)+len(value)+3 > limit {
			return ErrEventTooLarge
		}
		out.WriteString(name)
		out.WriteString(": ")
		out.WriteString(value)
		out.WriteByte('\n')
		return nil
	}
	// Omitted optional fields retain normal browser behavior. In particular, an
	// empty ID here does not silently reset the browser's Last-Event-ID cursor.
	for _, field := range []struct{ name, value string }{{"id", event.ID}, {"event", event.Name}} {
		if field.value != "" {
			if err := appendField(field.name, field.value); err != nil {
				return nil, err
			}
		}
	}
	if event.Retry > 0 {
		// SSE retry is decimal integer milliseconds, never Go's "1s" text format.
		if err := appendField("retry", strconv.FormatInt(event.Retry.Milliseconds(), 10)); err != nil {
			return nil, err
		}
	}
	// SSE recognizes CR, LF and CRLF as line endings. Normalize all three and
	// prefix every line, including the final empty line, to preserve multiline data.
	data := strings.ReplaceAll(strings.ReplaceAll(string(event.Data), "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(data, "\n") {
		if err := appendField("data", line); err != nil {
			return nil, err
		}
	}
	if out.Len()+1 > limit {
		return nil, ErrEventTooLarge
	}
	// The extra blank line dispatches the assembled event in the browser. Without
	// it, an otherwise flushed frame would still wait in the client's parser.
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// A bounded writer avoids retaining an oversized JSON result in a second buffer.
// Callers should still avoid constructing arbitrarily large values before SendJSON.
type jsonBuffer struct {
	bytes.Buffer
	limit int
}

func (b *jsonBuffer) Write(p []byte) (int, error) {
	// Return an error without a partial append; callers never send a truncated
	// JSON document that happens to fit the remaining frame budget.
	if len(p) > b.limit-b.Len() {
		return 0, ErrEventTooLarge
	}
	return b.Buffer.Write(p)
}

func encodeJSON(value any, limit int) ([]byte, error) {
	// JSON's own escaped newlines remain inside one data line. Ordinary Send is
	// the separate path for applications that intentionally need multiline text.
	b := &jsonBuffer{limit: limit}
	if err := json.NewEncoder(b).Encode(value); err != nil {
		if errors.Is(err, ErrEventTooLarge) {
			return nil, ErrEventTooLarge
		}
		// Marshaler errors may contain secrets; return only a stable public category.
		return nil, ErrInvalidEvent
	}
	// Encoder appends a newline for JSON streams; SSE adds its own delimiter.
	// Removing only that newline preserves any newline escaped inside the value.
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}
