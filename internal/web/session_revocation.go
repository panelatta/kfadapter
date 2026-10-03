package web

import (
	"context"
	"time"
)

func (s *sessionStore) revoke(ctx context.Context, cookieToken string) error {
	if cookieToken == "" {
		return nil
	}
	token := sessionKey(cookieToken)
	// The in-memory session and SSE streams are invalidated before touching
	// storage. Queue the deletion before attempting it so cancellation cannot
	// forget an already revoked session.
	s.mu.Lock()
	s.removeSessionLocked(token)
	if s.persistence != nil {
		s.undeleted[token] = struct{}{}
	}
	s.mu.Unlock()
	if s.persistence == nil {
		return nil
	}

	// Retry only inside this request, with a bounded, cancelable backoff. There
	// is no detached worker that can outlive HTTP shutdown or database closure.
	var lastErr error
	for _, delay := range [...]time.Duration{0, 50 * time.Millisecond, 150 * time.Millisecond} {
		if delay != 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		// Include earlier failed revocations: a later successful lock must not
		// silently leave an older known session able to return after a restart.
		lastErr = s.persistBrowserSessionDeletes(ctx, nil)
		if lastErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return lastErr
}

// persistBrowserSessionDeletes makes one pass over expired and explicitly
// revoked sessions. Explicit lock requests supply their cancellation context
// so retries also stop between deletions.
func (s *sessionStore) persistBrowserSessionDeletes(ctx context.Context, tokens []string) error {
	if s.persistence == nil {
		return nil
	}
	s.mu.Lock()
	for _, token := range tokens {
		if token != "" {
			s.undeleted[token] = struct{}{}
		}
	}
	pending := make([]string, 0, len(s.undeleted))
	for token := range s.undeleted {
		pending = append(pending, token)
	}
	s.mu.Unlock()
	var result error
	for _, token := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.persistence.DeleteBrowserSession(token)
		s.mu.Lock()
		if err == nil {
			delete(s.undeleted, token)
		} else {
			result = err
		}
		s.mu.Unlock()
	}
	return result
}
