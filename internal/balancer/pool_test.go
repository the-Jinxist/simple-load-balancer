package balancer

import (
	"sync"
	"testing"
)

// testBackend builds a Backend without starting the health-check goroutine so
// tests can control Alive directly.
func testBackend(alive bool) *Backend {
	return &Backend{Alive: alive}
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
// backends and wraps around to the start of the slice. The cursor advances past
// the selected backend, so each alive backend is selected once per cycle.
func TestServerPoolNextPeerSkipsDead(t *testing.T) {
	backends := []*Backend{
		testBackend(false),
		testBackend(true),
		testBackend(false),
		testBackend(true),
	}
	pool := &ServerPool{servers: backends}

	want := []*Backend{
		backends[1], backends[3],
		backends[1], backends[3],
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

// TestServerPoolNextPeerAdvancesPastSelected verifies that a fallback scan
// advances the cursor past the backend it selected, not merely past the index
// where the scan started.
func TestServerPoolNextPeerAdvancesPastSelected(t *testing.T) {
	backends := []*Backend{
		testBackend(true),  // 0
		testBackend(false), // 1
		testBackend(true),  // 2
		testBackend(false), // 3
	}
	pool := &ServerPool{servers: backends}

	if got := pool.NextPeer(); got != backends[0] {
		t.Fatalf("first NextPeer() = %p, want backends[0] (%p)", got, backends[0])
	}
	if got := pool.current.Load(); got != 1 {
		t.Fatalf("after first selection current = %d, want 1", got)
	}

	// The scan starts at 1, skips it, selects index 2, and should leave the
	// cursor at index 3.
	if got := pool.NextPeer(); got != backends[2] {
		t.Fatalf("second NextPeer() = %p, want backends[2] (%p)", got, backends[2])
	}
	if got := pool.current.Load(); got != 3 {
		t.Fatalf("after second selection current = %d, want 3", got)
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
