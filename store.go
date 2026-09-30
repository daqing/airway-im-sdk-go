package airwayim

import "sync"

// SequenceStore is the key-value seam for persisting per-conversation
// last-seen sequences. The default in-process store is
// NewInMemorySequenceStore; provide your own (file, database, redis, …)
// via WithSequenceStore so a restart only fetches the delta.
type SequenceStore interface {
	Get(key string) (string, bool)
	Set(key, value string)
	Remove(key string)
}

// InMemorySequenceStore keeps cursors in process memory.
type InMemorySequenceStore struct {
	mu     sync.RWMutex
	values map[string]string
}

// NewInMemorySequenceStore returns an empty in-memory store.
func NewInMemorySequenceStore() *InMemorySequenceStore {
	return &InMemorySequenceStore{values: make(map[string]string)}
}

func (s *InMemorySequenceStore) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.values[key]
	return value, ok
}

func (s *InMemorySequenceStore) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[key] = value
}

func (s *InMemorySequenceStore) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
}
