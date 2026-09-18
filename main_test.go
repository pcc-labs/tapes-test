package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestEachBoundsConcurrencyAndReturnsAnError(t *testing.T) {
	var inFlight, peak, ran atomic.Int32
	boom := errors.New("boom")
	err := each(12, 3, func(i int) error {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		ran.Add(1)
		if i == 4 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	if ran.Load() != 12 {
		t.Errorf("ran %d of 12: every call should finish", ran.Load())
	}
	if p := peak.Load(); p > 3 || p < 2 {
		t.Errorf("peak in flight = %d, want 2..3", p)
	}
}

func TestTruncateKeepsWholeCharacters(t *testing.T) {
	if got := truncate("when a user 👍 on the digest", 14); got != "when a user 👍 " {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("short", 48); got != "short" {
		t.Errorf("truncate = %q", got)
	}
}
