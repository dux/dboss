package throttle

import (
	"testing"
	"time"
)

func clocked() (*Throttle, *time.Time) {
	now := time.Unix(1_000_000, 0)
	throttle := New()
	throttle.Now = func() time.Time { return now }
	return throttle, &now
}

// Checks from one IP are spaced Spacing apart, parallel ones queue, and past MaxWait of queue
// the check is refused at once.
func TestReserveQueuesPerIP(t *testing.T) {
	throttle, now := clocked()
	for i, want := range []time.Duration{0, 3 * time.Second, 6 * time.Second} {
		if slot, ok := throttle.Reserve("192.0.2.1"); !ok || slot.Wait != want {
			t.Fatalf("check %d waits %v %v, want %v", i, slot.Wait, ok, want)
		}
	}
	if slot, ok := throttle.Reserve("192.0.2.2"); !ok || slot.Wait != 0 {
		t.Fatalf("another IP waits %v %v", slot.Wait, ok)
	}
	for range 8 {
		throttle.Reserve("192.0.2.1")
	}
	if slot, ok := throttle.Reserve("192.0.2.1"); ok || slot.Wait <= MaxWait {
		t.Fatalf("past the queue cap = %v %v", slot.Wait, ok)
	}
	// A minute on, the queue has drained and the next reserve sweeps the drained IPs first.
	*now = now.Add(time.Minute)
	if slot, ok := throttle.Reserve("192.0.2.1"); !ok || slot.Wait != 0 {
		t.Fatalf("after the queue drained = %v %v", slot.Wait, ok)
	}
	if throttle.Len() != 1 {
		t.Fatalf("sweep kept %d IPs, want only the new booking", throttle.Len())
	}
}

// A released slot leaves the next check immediate; one booked behind it keeps its place.
func TestReleaseRefundsOnlyTheLastSlot(t *testing.T) {
	throttle, _ := clocked()
	for range 3 {
		slot, _ := throttle.Reserve("192.0.2.1")
		throttle.Release(slot)
	}
	if slot, _ := throttle.Reserve("192.0.2.1"); slot.Wait != 0 {
		t.Fatalf("passing checks queued: %v", slot.Wait)
	}
	first, _ := throttle.Reserve("192.0.2.1")
	throttle.Reserve("192.0.2.1")
	throttle.Release(first)
	if slot, _ := throttle.Reserve("192.0.2.1"); slot.Wait != 9*time.Second {
		t.Fatalf("release behind a queued slot = %v", slot.Wait)
	}
}
