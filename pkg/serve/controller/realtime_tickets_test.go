package controller

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTicketsSingleUseExpiryAndConcurrentConsumption(t *testing.T) {
	store := newTicketStore()
	now := time.Unix(100, 0)
	ticket, err := store.issue("private-test-token", now)
	if err != nil || len(ticket) != 43 {
		t.Fatal("issue random 32-byte ticket failed")
	}
	var winners atomic.Int32
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			token, err := store.consume(ticket, now)
			if err == nil {
				if token != "private-test-token" {
					t.Error("ticket did not retain its original session")
				}
				winners.Add(1)
			} else if !errors.Is(err, errTicketInvalid) {
				t.Error("unexpected consumption error")
			}
		}()
	}
	wait.Wait()
	if winners.Load() != 1 {
		t.Fatalf("ticket consumed %d times", winners.Load())
	}
	expired, _ := store.issue("another-private-token", now)
	if _, err := store.consume(expired, now.Add(ticketTTL)); !errors.Is(err, errTicketInvalid) {
		t.Fatal("ticket accepted at its expiry boundary")
	}
	for _, bad := range []string{"", "short", ticket} {
		if _, err := store.consume(bad, now); !errors.Is(err, errTicketInvalid) {
			t.Fatal("invalid or previously consumed ticket accepted")
		}
	}
}

func TestTicketCapacityIsBoundedAndExpiredEntriesAreReclaimed(t *testing.T) {
	store := newTicketStore()
	now := time.Unix(200, 0)
	var accepted atomic.Int32
	var wait sync.WaitGroup
	for range ticketCapacity + 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := store.issue("private-test-token", now); err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, errTicketCapacity) {
				t.Error("unexpected capacity error")
			}
		}()
	}
	wait.Wait()
	if accepted.Load() != ticketCapacity || len(store.entries) != ticketCapacity {
		t.Fatal("concurrent issue exceeded or undershot ticket capacity")
	}
	if _, err := store.issue("private-test-token", now.Add(ticketTTL)); err != nil || len(store.entries) != 1 {
		t.Fatal("expired tickets were not reclaimed lazily")
	}
	if _, err := store.issue("", now); !errors.Is(err, errTicketInvalid) {
		t.Fatal("ticket without an underlying session was accepted")
	}
}
