package api

import (
	"context"
	"sync"
)

// A read scope lets a runtime answer the reads of one plan from one look at
// its machines. The planner opens one around its capacity check, which asks
// Capacity and then LargestFitFor; a runtime whose answer to both comes from
// one expensive read (every pod in a cluster) keeps that read for the rest of
// the scope rather than making it twice.
//
// Nothing read in a scope outlives it, so no plan is answered from what an
// earlier plan read: each plan's capacity is as current as the moment it is
// checked, which is what R-242's refusal rests on.
type readScope struct {
	mu   sync.Mutex
	read map[any]any
}

type readScopeKey struct{}

// WithReadScope opens a read scope for the reads made with the context it
// returns.
func WithReadScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, readScopeKey{}, &readScope{read: map[any]any{}})
}

// ScopedRead is read's answer, made once per read scope and key. Outside a
// scope, or when read fails, nothing is kept and every call reads.
func ScopedRead[T any](ctx context.Context, key any, read func() (T, error)) (T, error) {
	s, _ := ctx.Value(readScopeKey{}).(*readScope)
	if s == nil {
		return read()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.read[key].(T); ok {
		return v, nil
	}
	v, err := read()
	if err == nil {
		s.read[key] = v
	}
	return v, err
}
