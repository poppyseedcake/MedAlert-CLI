package medicover

import (
	"context"
	"sync"
)

// Coordinator serializes session renewal for one account so concurrent
// watchers share a single refresh operation. Different accounts use
// independent entries and never block each other.
type Coordinator struct {
	mu       sync.Mutex
	inflight map[string]*inflightRefresh
}

type inflightRefresh struct {
	done   chan struct{}
	tokens Tokens
	state  *SessionState
	err    error
}

// NewCoordinator returns an empty renewal coordinator.
func NewCoordinator() *Coordinator {
	return &Coordinator{inflight: map[string]*inflightRefresh{}}
}

// RefreshOne performs at most one refresh HTTP call for concurrent callers
// with the same account id. Callers share the same result.
func (k *Coordinator) RefreshOne(ctx context.Context, client *Client, accountID string, session *SessionState) (Tokens, *SessionState, error) {
	k.mu.Lock()
	if existing, ok := k.inflight[accountID]; ok {
		k.mu.Unlock()
		select {
		case <-ctx.Done():
			return Tokens{}, nil, temporary("renewal was cancelled")
		case <-existing.done:
			return existing.tokens, existing.state, existing.err
		}
	}
	call := &inflightRefresh{done: make(chan struct{})}
	k.inflight[accountID] = call
	k.mu.Unlock()

	tokens, state, err := client.Refresh(ctx, session)

	k.mu.Lock()
	call.tokens, call.state, call.err = tokens, state, err
	delete(k.inflight, accountID)
	close(call.done)
	k.mu.Unlock()
	return tokens, state, err
}
