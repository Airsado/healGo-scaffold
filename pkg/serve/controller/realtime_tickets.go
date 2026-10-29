package controller

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

const ticketTTL = 30 * time.Second
const ticketCapacity = 128

// These categories never include the credential. The HTTP handler selects
// retryable capacity errors without exposing ticket or underlying bearer bytes.
var errTicketInvalid = errors.New("invalid realtime ticket")
var errTicketCapacity = errors.New("realtime ticket capacity reached")
var errTicketRandom = errors.New("realtime ticket unavailable")

type ticketEntry struct {
	token   string
	expires time.Time
}

// ticketStore is intentionally private and process-local. Issuing a ticket does
// not create a second authentication system: consumption must revalidate the
// original bearer session, including logout, expiry and current permissions.
type ticketStore struct {
	mu      sync.Mutex
	entries map[string]ticketEntry
}

func newTicketStore() *ticketStore { return &ticketStore{entries: make(map[string]ticketEntry)} }

func (s *ticketStore) issue(token string, now time.Time) (string, error) {
	if token == "" {
		return "", errTicketInvalid
	}
	// Random bytes are generated before taking the mutex; map mutation and
	// the capacity decision share one lock, preventing concurrent overbooking.
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errTicketRandom
	}
	ticket := base64.RawURLEncoding.EncodeToString(random[:])
	// URL-safe encoding avoids an extra escaping format; all 256 random bits
	// remain present. The map stores this opaque value, never a caller-chosen ID.
	s.mu.Lock()
	defer s.mu.Unlock()
	// Lazy cleanup needs no permanent sweeper goroutine. The bounded map keeps
	// this scan predictable even if a client never consumes issued tickets.
	for key, entry := range s.entries {
		if !now.Before(entry.expires) {
			delete(s.entries, key)
		}
	}
	if len(s.entries) >= ticketCapacity {
		// This is ticket capacity, distinct from active stream capacity. A ticket
		// can expire unused and must not reserve a long-lived network session.
		return "", errTicketCapacity
	}
	if _, duplicate := s.entries[ticket]; duplicate {
		return "", errTicketRandom
	}
	s.entries[ticket] = ticketEntry{token: token, expires: now.Add(ticketTTL)}
	return ticket, nil
}

func (s *ticketStore) consume(ticket string, now time.Time) (string, error) {
	// A 32-byte raw URL-safe Base64 credential is always 43 bytes. Rejecting
	// arbitrary lengths avoids retaining or processing unbounded query input.
	if len(ticket) != 43 {
		return "", errTicketInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[ticket]
	// Entries retain the original session only in memory. After returning the
	// token, the controller immediately checks whether that session is still valid.
	// Delete before validating expiry so even concurrent handshakes cannot
	// reuse a ticket. A failed upgrade requires a newly issued credential.
	delete(s.entries, ticket)
	if !exists || !now.Before(entry.expires) {
		return "", errTicketInvalid
	}
	return entry.token, nil
}
