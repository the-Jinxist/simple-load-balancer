package balancer

import "sync/atomic"

// serverPool is the process-wide pool used by LoadBalance. It is initialized
// once by InitializePool. See IMPROVEMENTS.md (P2-15) for why a package-level
// global is not ideal.
var serverPool *ServerPool

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
func InitializePool(servers []*Backend) {
	serverPool = &ServerPool{
		servers: servers,
	}
}

// GetServerPool returns the global pool, or nil if InitializePool has not been
// called yet.
func GetServerPool() *ServerPool {
	return serverPool
}

// NextIndex advances the cursor and returns the next index into servers.
//
// The cursor is incremented before the modulo, so the first call returns 1
// rather than 0. NextIndex panics when the pool is empty. See IMPROVEMENTS.md
// (P1-6, P1-7).
func (s *ServerPool) NextIndex() int {
	return int(s.current.Add(1) % uint64(len(s.servers)))
}

// NextPeer returns the next alive backend in round-robin order, starting at
// NextIndex() and scanning at most one full cycle of the pool. It returns nil
// when every backend is dead.
//
// When the scan skips one or more dead backends, the raw loop index is stored
// back into the cursor instead of the normalized backend index. See
// IMPROVEMENTS.md (P1-6).
func (s *ServerPool) NextPeer() *Backend {
	// servers = [1, 2, 3, 4, 5]
	// we're on server 3
	//
	// If the next pick is dead, walk forward through the slice (wrapping with
	// modulo) until an alive backend is found or the full cycle is exhausted.
	nextIndex := s.NextIndex()
	// nextIndex = 4

	l := len(s.servers) + nextIndex
	// full cycle = 5 + 4 = 9

	for i := nextIndex; i < l; i++ {
		idx := i % len(s.servers)
		if s.servers[idx].IsAlive() {
			if i != nextIndex {
				s.current.Store(uint64(i)) // mark the current one (raw, unnormalized index)
			}
			return s.servers[idx]
		}
	}

	return nil
}
