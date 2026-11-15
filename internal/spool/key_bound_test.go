package spool

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A call that cannot be interrupted is waited for only as long as its
// context lasts; one that answers is passed through.
func TestWithinBoundsACallThatNeverReturns(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := within(ctx, func() (string, error) { <-block; return "late", nil })
	if !errors.Is(err, ErrKeychainTimeout) || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
	got, err := within(context.Background(), func() (string, error) { return "v", nil })
	if got != "v" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
	want := errors.New("x")
	if _, err := within(context.Background(), func() (string, error) { return "", want }); !errors.Is(err, want) {
		t.Fatalf("%v", err)
	}
}
