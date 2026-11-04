package balancer

import (
	"sync"
	"testing"
)

// testBackend builds a Backend without starting the health-check goroutine,
// which is currently broken (see P2-14 in IMPROVEMENTS.md).
func testBackend(alive bool) *Backend {
	return &Backend{Alive: alive}
}

// TestServerPoolNextIndexSequence documents the current cursor arithmetic:
// the counter is pre-incremented and decremented again, so the first index is 0.
func TestServerPoolNextIndexSequence(t *testing.T) {
	pool := &ServerPool{
		servers: []*Backend{testBackend(true), testBackend(true), testBackend(true)},
	}

	want := []int{0, 1, 2, 0, 1, 2}
	for i, expected := range want {
		if got := pool.NextIndex(); got != expected {
			t.Fatalf("call %d: NextIndex() = %d, want %d", i+1, got, expected)
		}
	}
}

// TestServerPoolNextPeerRoundRobin verifies that consecutive calls rotate
// through every alive backend without repeating one before the full cycle.
func TestServerPoolNextPeerRoundRobin(t *testing.T) {
	backends := []*Backend{
		testBackend(true),
		testBackend(true),
		testBackend(true),
	}
	pool := &ServerPool{servers: backends}

	want := []*Backend{
		backends[0], backends[1], backends[2],
		backends[0], backends[1], backends[2],
	}

	for i, expected := range want {
		got := pool.NextPeer()
		if got != expected {
			t.Fatalf("call %d: NextPeer() = %p, want %p", i+1, got, expected)
		}
	}
}

// TestServerPoolNextPeerSkipsDead verifies that the scan moves past dead
// backends and wraps around to the start of the slice. Each alive backend is
// currently selected twice in a row because a fallback scan stores the selected
// index while NextIndex computes the previous cursor value.
func TestServerPoolNextPeerSkipsDead(t *testing.T) {
	backends := []*Backend{
		testBackend(false),
		testBackend(true),
		testBackend(false),
		testBackend(true),
	}
	pool := &ServerPool{servers: backends}

	want := []*Backend{
		backends[1], backends[1],
		backends[3], backends[3],
		backends[1], backends[1],
	}

	for i, expected := range want {
		got := pool.NextPeer()
		if got != expected {
			t.Fatalf("call %d: NextPeer() = %p, want %p", i+1, got, expected)
		}
	}
}

// TestServerPoolNextPeerAllDead verifies that a pool with no healthy backends
// returns nil instead of selecting a dead one.
func TestServerPoolNextPeerAllDead(t *testing.T) {
	pool := &ServerPool{
		servers: []*Backend{testBackend(false), testBackend(false), testBackend(false)},
	}

	if got := pool.NextPeer(); got != nil {
		t.Fatalf("NextPeer() = %p, want nil", got)
	}
}

// TestServerPoolNextPeerEmptyPool verifies that selecting from an empty pool
// returns nil instead of dividing by zero.
func TestServerPoolNextPeerEmptyPool(t *testing.T) {
	pool := &ServerPool{}

	if got := pool.NextPeer(); got != nil {
		t.Fatalf("NextPeer() = %p, want nil", got)
	}
}

// TestServerPoolNextPeerNormalizesIndex verifies that a fallback scan stores the
// normalized backend index in the cursor rather than the raw loop index.
func TestServerPoolNextPeerNormalizesIndex(t *testing.T) {
	backends := []*Backend{
		testBackend(true),
		testBackend(false),
		testBackend(false),
		testBackend(false),
	}
	pool := &ServerPool{servers: backends}

	// Start the cursor at 1 so the scan must wrap past the end of the slice
	// (i == 4, idx == 0) to find the only alive backend.
	pool.current.Store(1)

	if got := pool.NextPeer(); got != backends[0] {
		t.Fatalf("NextPeer() = %p, want backends[0] (%p)", got, backends[0])
	}

	if got := pool.current.Load(); got != 0 {
		t.Fatalf("current = %d, want normalized index 0", got)
	}
}

// TestServerPoolNextPeerConcurrent exercises concurrent selections. All
// backends are alive, so this only reads the slice and updates the atomic
// cursor; run with -race to check for data races.
func TestServerPoolNextPeerConcurrent(t *testing.T) {
	backends := []*Backend{
		testBackend(true),
		testBackend(true),
		testBackend(true),
		testBackend(true),
	}
	pool := &ServerPool{servers: backends}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if peer := pool.NextPeer(); peer == nil {
					t.Error("NextPeer() returned nil with all backends alive")
					return
				}
			}
		}()
	}
	wg.Wait()
}
