package balancer

import (
	"sync"
	"testing"
)

// testBackend builds a Backend without starting the health-check goroutine, so
// tests can control Alive directly. NewBackend is not used because it currently
// panics on startup (see IMPROVEMENTS.md P0-1).
func testBackend(alive bool) *Backend {
	return &Backend{Alive: alive}
}

// TestServerPoolNextIndexSequence documents the current cursor arithmetic:
// the counter is incremented before the modulo, so the first index is 1, not 0.
// When IMPROVEMENTS.md P1-6 is addressed this test should expect 0 first.
func TestServerPoolNextIndexSequence(t *testing.T) {
	pool := &ServerPool{
		servers: []*Backend{testBackend(true), testBackend(true), testBackend(true)},
	}

	want := []int{1, 2, 0, 1, 2, 0}
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

	// The first selection is index 1 because of current NextIndex behavior.
	want := []*Backend{
		backends[1], backends[2], backends[0],
		backends[1], backends[2], backends[0],
	}

	for i, expected := range want {
		got := pool.NextPeer()
		if got != expected {
			t.Fatalf("call %d: NextPeer() = %p, want %p", i+1, got, expected)
		}
	}
}

// TestServerPoolNextPeerSkipsDead verifies that the scan moves past dead
// backends and wraps around to the start of the slice.
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

// TestServerPoolNextPeerEmptyPoolPanics documents a known issue: the modulo in
// NextIndex divides by zero when the pool has no backends. When
// IMPROVEMENTS.md P1-7 is fixed, this test should assert a nil return instead.
func TestServerPoolNextPeerEmptyPoolPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected NextPeer on an empty pool to panic")
		}
	}()

	pool := &ServerPool{}
	pool.NextPeer()
}

// TestServerPoolNextPeerStoresRawIndex documents a known issue: after a
// fallback scan, the cursor is stored as the raw loop index (which can be
// larger than len(servers)) rather than the normalized backend index. When
// IMPROVEMENTS.md P1-6 is fixed this test should assert a normalized value.
func TestServerPoolNextPeerStoresRawIndex(t *testing.T) {
	backends := []*Backend{
		testBackend(true),
		testBackend(false),
		testBackend(false),
		testBackend(false),
	}
	pool := &ServerPool{servers: backends}

	if got := pool.NextPeer(); got != backends[0] {
		t.Fatalf("NextPeer() = %p, want backends[0] (%p)", got, backends[0])
	}

	// The scan selected i == 4 (index 0 after the modulo) and stored 4.
	if got := pool.current.Load(); got != 4 {
		t.Fatalf("current = %d, want raw index 4", got)
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
