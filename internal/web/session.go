package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

type browserSession struct {
	expiresAt time.Time
	done      chan struct{}
}

type sessionStore struct {
	mu             sync.Mutex
	sessions       map[string]browserSession
	pending        map[string]struct{}
	accessAttempts map[string]accessAttempt
	loginAttempts  map[string]loginAttempt
	persistence    BrowserSessionPersistence
	// undeleted holds revoked session keys awaiting durable deletion. Locking
	// revokes the in-memory session first, so a storage failure can only
	// delay, never cancel, the deletion.
	undeleted  map[string]struct{}
	now        func() time.Time
	random     io.Reader
	ttl        time.Duration
	max        int
	generation uint64
}

func newSessionStore(now func() time.Time, random io.Reader, ttl time.Duration, max int, persistence BrowserSessionPersistence) (*sessionStore, error) {
	if now == nil {
		now = time.Now
	}
	if random == nil {
		random = rand.Reader
	}
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	if max <= 0 {
		max = 128
	}
	store := &sessionStore{
		sessions:       make(map[string]browserSession),
		pending:        make(map[string]struct{}),
		accessAttempts: make(map[string]accessAttempt),
		loginAttempts:  make(map[string]loginAttempt),
		persistence:    persistence,
		undeleted:      make(map[string]struct{}),
		now:            now,
		random:         random,
		ttl:            ttl,
		max:            max,
	}
	if persistence == nil {
		return store, nil
	}
	restoredAt := now().UTC()
	var discarded [][]byte
	if err := persistence.RestoreBrowserSessions(context.Background(), restoredAt, max, func(tokenHash []byte, expiresAt time.Time) error {
		if expiresAt.After(restoredAt.Add(ttl)) {
			discarded = append(discarded, append([]byte(nil), tokenHash...))
			return nil
		}
		return store.restore(restoredAt, string(tokenHash), expiresAt)
	}); err != nil {
		return nil, err
	}
	for _, tokenHash := range discarded {
		if err := persistence.DeleteBrowserSession(context.Background(), tokenHash); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *sessionStore) restore(now time.Time, token string, expiresAt time.Time) error {
	if len(token) != sha256.Size || !now.Before(expiresAt) || expiresAt.After(now.Add(s.ttl)) {
		return errors.New("invalid persisted browser session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= s.max {
		return errors.New("restored browser session limit exceeded")
	}
	if _, exists := s.sessions[token]; exists {
		return errors.New("duplicate persisted browser session")
	}
	s.sessions[token] = browserSession{expiresAt: expiresAt.UTC(), done: make(chan struct{})}
	return nil
}

func (s *sessionStore) create(ctx context.Context) (string, browserSession, error) {
	// Entropy reads are deliberately outside s.mu: a blocked entropy source must
	// not prevent a concurrent access request from observing its admission.
	for range 4 {
		token, err := randomToken(s.random, 32)
		if err != nil {
			return "", browserSession{}, err
		}
		session := browserSession{expiresAt: s.now().UTC().Add(s.ttl), done: make(chan struct{})}
		key := sessionKey(token)

		s.mu.Lock()
		expired := s.pruneLocked()
		generation := s.generation
		if len(s.sessions)+len(s.pending) >= s.max {
			s.mu.Unlock()
			s.deleteBrowserSessions(ctx, expired)
			return "", browserSession{}, errors.New("browser session limit reached")
		}
		if _, collision := s.sessions[key]; collision {
			s.mu.Unlock()
			s.deleteBrowserSessions(ctx, expired)
			continue
		}
		if _, collision := s.pending[key]; collision {
			s.mu.Unlock()
			s.deleteBrowserSessions(ctx, expired)
			continue
		}
		if _, revoked := s.undeleted[key]; revoked {
			s.mu.Unlock()
			s.deleteBrowserSessions(ctx, expired)
			continue
		}
		s.pending[key] = struct{}{}
		s.mu.Unlock()
		s.deleteBrowserSessions(ctx, expired)

		if s.persistence != nil {
			err = s.persistence.SaveBrowserSession(ctx, token, session.expiresAt, s.max)
		}
		s.mu.Lock()
		delete(s.pending, key)
		usable := err == nil && generation == s.generation && s.now().Before(session.expiresAt)
		if usable {
			s.sessions[key] = session
		}
		s.mu.Unlock()
		if err != nil {
			return "", browserSession{}, err
		}
		if !usable {
			s.deleteBrowserSessions(ctx, []string{key})
			return "", browserSession{}, errors.New("browser session expired before persistence")
		}
		return token, session, nil
	}
	return "", browserSession{}, errors.New("browser session token collision")
}

func (s *sessionStore) valid(ctx context.Context, token string) (browserSession, bool) {
	if token == "" {
		return browserSession{}, false
	}
	token = sessionKey(token)
	s.mu.Lock()
	session, ok := s.sessions[token]
	expired := ok && !s.now().Before(session.expiresAt)
	if !ok || expired {
		if expired {
			s.removeSessionLocked(token)
		}
		s.mu.Unlock()
		if expired {
			s.deleteBrowserSessions(ctx, []string{token})
		}
		return browserSession{}, false
	}
	s.mu.Unlock()
	return session, true
}

// revoke invalidates the in-memory session and its event streams before
// touching storage, then retries the durable deletion within this request.
func (s *sessionStore) revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	token = sessionKey(token)
	// Queue the deletion before attempting it so cancellation cannot forget an
	// already revoked session.
	s.mu.Lock()
	s.removeSessionLocked(token)
	if s.persistence != nil {
		s.undeleted[token] = struct{}{}
	}
	s.mu.Unlock()
	// Include earlier failed revocations: a later successful lock must not
	// silently leave an older known session able to return after a restart.
	return s.retryPersistence(ctx, func() error { return s.persistBrowserSessionDeletes(ctx, nil) })
}

// retryPersistence retries a durable write with a bounded, cancelable backoff
// inside the calling request; no detached worker outlives HTTP shutdown.
func (s *sessionStore) retryPersistence(ctx context.Context, write func() error) error {
	if s.persistence == nil {
		return nil
	}
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
		if lastErr = write(); lastErr == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return lastErr
}

func (s *sessionStore) deleteBrowserSessions(ctx context.Context, tokens []string) {
	_ = s.persistBrowserSessionDeletes(ctx, tokens)
}

// persistBrowserSessionDeletes makes one pass over tokens plus every revoked
// session still awaiting durable deletion.
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
		err := s.persistence.DeleteBrowserSession(ctx, []byte(token))
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

// matches verifies that the exact session which started a stream remains
// active. It also closes the session done signal at expiry.
func (s *sessionStore) matches(ctx context.Context, token string, expected browserSession) bool {
	if token == "" || expected.done == nil {
		return false
	}
	token = sessionKey(token)
	s.mu.Lock()
	session, ok := s.sessions[token]
	expired := ok && !s.now().Before(session.expiresAt)
	matches := ok && session.done == expected.done && !expired
	if expired {
		s.removeSessionLocked(token)
	}
	s.mu.Unlock()
	if expired {
		s.deleteBrowserSessions(ctx, []string{token})
	}
	return matches
}

func sessionKey(token string) string {
	hash := sha256.Sum256([]byte(token))
	return string(hash[:])
}

func (s *sessionStore) revokeAll(ctx context.Context) error {
	s.mu.Lock()
	s.generation++
	for key := range s.sessions {
		s.removeSessionLocked(key)
	}
	clear(s.pending)
	clear(s.loginAttempts)
	s.mu.Unlock()
	return s.retryPersistence(ctx, func() error {
		if err := s.persistence.DeleteAllBrowserSessions(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		clear(s.undeleted)
		s.mu.Unlock()
		return nil
	})
}

func (s *sessionStore) removeSessionLocked(token string) {
	if session, ok := s.sessions[token]; ok {
		delete(s.sessions, token)
		if session.done != nil {
			close(session.done)
		}
	}
	delete(s.loginAttempts, token)
}

func (s *sessionStore) pruneLocked() []string {
	now := s.now()
	var expired []string
	for token, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			s.removeSessionLocked(token)
			expired = append(expired, token)
		}
	}
	for token := range s.loginAttempts {
		if _, active := s.sessions[token]; !active {
			delete(s.loginAttempts, token)
		}
	}
	s.trimAccessAttemptsLocked()
	s.trimLoginAttemptsLocked()
	return expired
}

func randomToken(random io.Reader, bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (a *API) authenticate(r *http.Request) (string, browserSession, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", browserSession{}, false
	}
	session, ok := a.sessions.valid(r.Context(), cookie.Value)
	return cookie.Value, session, ok
}

func (a *API) revokeBrowserSession(ctx context.Context, token string) error {
	return a.sessions.revoke(ctx, token)
}

// Cookies issued through the TLS proxy are Secure; direct local HTTP
// sessions stay usable.
func setSessionCookie(w http.ResponseWriter, token string, expires time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/api/v1",
		Expires:  expires,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/api/v1",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}
