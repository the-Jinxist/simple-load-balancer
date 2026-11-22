package balancer

import "sync/atomic"

// ServerPool holds the set of backends and a rotating cursor used to implement
// round-robin selection.
type ServerPool struct {
	servers []*Backend
	// current is the next index to try. It is only written by the CAS loop in
	// NextPeer and is always normalized to [0, len(servers)).
	current atomic.Uint64
}

// InitializePool creates a ServerPool from servers.
func InitializePool(servers []*Backend) *ServerPool {
	return &ServerPool{
		servers: servers,
	}
}

// NextPeer returns the next alive backend in round-robin order.
//
// It scans at most one full cycle starting at the current cursor, then advances
// the cursor past the selected backend with a CAS. If another goroutine changes
// the cursor first, it retries. It returns nil when every backend is dead.
func (s *ServerPool) NextPeer() *Backend {
	n := len(s.servers)
	if n == 0 {
		return nil
	}

	for {
		current := int(s.current.Load())
		selected := -1

		// Scan at most one full cycle, starting at the current cursor.
		path := (current + n) - 1
		for i := current; i <= path; i++ {
			idx := i % n
			if s.servers[idx].IsAlive() {
				selected = idx
				break
			}
		}

		if selected == -1 {
			return nil
		}

		next := uint64((selected + 1) % n)
		if s.current.CompareAndSwap(uint64(current), next) {
			return s.servers[selected]
		}

	}
}

// SelectedBacked returns the backend at the current cursor position. The name
// is misspelled; see IMPROVEMENTS.md (P2-16).
func (s *ServerPool) SelectedBacked() *Backend {
	return s.servers[s.current.Load()]
}
