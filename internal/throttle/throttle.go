// Package throttle spaces secret checks per client IP, so a password or a token cannot be guessed
// fast. It lives in memory only, so a restart starts every IP fresh.
package throttle

import (
	"sync"
	"time"
)

const (
	// Spacing is the least time between two checks from one client IP; parallel checks queue
	// instead of slipping through.
	Spacing = 3 * time.Second
	// MaxWait caps that queue: a check that would wait longer is refused at once, so a flood
	// cannot pile up waiting connections.
	MaxWait = 30 * time.Second
	// sweepEvery is how often Reserve drops the IPs whose queue has drained.
	sweepEvery = time.Minute
)

// Slot is one booked check: Wait is how long the caller sleeps before it.
type Slot struct {
	ip   string
	at   time.Time
	Wait time.Duration
}

// Throttle hands out check slots per client IP. Now is injectable for tests.
type Throttle struct {
	mu      sync.Mutex
	next    map[string]time.Time
	swept   time.Time
	Spacing time.Duration
	Now     func() time.Time
}

func New() *Throttle {
	return &Throttle{next: map[string]time.Time{}, Spacing: Spacing, Now: time.Now}
}

// Reserve books the IP's next check slot. It answers false, with the wait, when the queue is past
// MaxWait; nothing is booked then.
func (t *Throttle) Reserve(ip string) (Slot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.Now()
	if now.Sub(t.swept) >= sweepEvery {
		t.sweep(now)
	}
	at := now
	if next, ok := t.next[ip]; ok && next.After(now) {
		at = next
	}
	slot := Slot{ip: ip, at: at, Wait: at.Sub(now)}
	if slot.Wait > MaxWait {
		return slot, false
	}
	t.next[ip] = at.Add(t.Spacing)
	return slot, true
}

// Release gives a slot back after a check that passed, so a client that never fails never waits.
// A slot booked behind it keeps the queue as it is.
func (t *Throttle) Release(slot Slot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.next[slot.ip].Equal(slot.at.Add(t.Spacing)) {
		t.next[slot.ip] = slot.at
	}
}

// sweep forgets the IPs whose next slot has passed; they would get an immediate check anyway.
func (t *Throttle) sweep(now time.Time) {
	t.swept = now
	for ip, next := range t.next {
		if !next.After(now) {
			delete(t.next, ip)
		}
	}
}

// Len is the number of IPs the throttle still tracks.
func (t *Throttle) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.next)
}
