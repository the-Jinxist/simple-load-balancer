package balancer

import "sync/atomic"

// ServerPool holds the set of backends and a rotating cursor used to implement
// round-robin selection.
type ServerPool struct {
	servers []*Backend
	// current is the last cursor value handed out by NextIndex. It is updated
	// atomically, but the Add/Store sequence in NextPeer is not atomic as a
	// whole. See IMPROVEMENTS.md (P1-6).
	current atomic.Uint64
}

// InitializePool replaces the global pool with servers. It must be called
// before LoadBalance handles any requests.
func InitializePool(servers []*Backend) *ServerPool {
	return &ServerPool{
		servers: servers,
	}
}

// NextPeer returns the next alive backend in round-robin order, starting at
// NextIndex() and scanning at most one full cycle of the pool. It returns nil
// when every backend is dead.
//
// When the scan skips one or more dead backends, the raw loop index is stored
// back into the cursor instead of the normalized backend index. See
// IMPROVEMENTS.md (P1-6).
func (s *ServerPool) NextPeer() *Backend {
	n := len(s.servers)
	if n == 0 {
		return nil
	}

	for {
		current := int(s.current.Load())
		selected := -1

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

func (s *ServerPool) SelectedBacked() *Backend {
	return s.servers[s.current.Load()]
}
