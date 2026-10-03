package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func assertRevocationPending(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed durable lock status = %d: %s", response.Code, response.Body.String())
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "session_revocation_pending" || !strings.Contains(problem.Detail, "after the service restarts") {
		t.Fatalf("failed durable lock was not explained: %+v", problem)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName || cookies[0].Value != "" || cookies[0].MaxAge >= 0 {
		t.Fatalf("failed durable lock did not clear the browser cookie: %v", cookies)
	}
}

func TestLockRetriesTransientDeletionBeforeReportingSuccess(t *testing.T) {
	persistence := &fakeBrowserSessionPersistence{}
	backend := &fakeBackend{}
	api := newPersistentTestAPI(t, time.Now, backend, persistence)
	cookie, csrf := establish(t, api)
	persistence.mu.Lock()
	persistence.deleteFailures = 2
	persistence.mu.Unlock()

	locked := requestWithCSRF(api, http.MethodPost, "/api/v1/access/logout", `{}`, cookie, csrf)
	if locked.Code != http.StatusNoContent {
		t.Fatalf("transient deletion failure was not recovered: %d %s", locked.Code, locked.Body.String())
	}
	persistence.mu.Lock()
	calls := persistence.deleteCalls
	_, remains := persistence.sessions[sessionKey(cookie.Value)]
	persistence.mu.Unlock()
	if calls != 3 || remains {
		t.Fatalf("successful lock deletion calls = %d, row remains = %v", calls, remains)
	}
	restarted := newPersistentTestAPI(t, time.Now, backend, persistence)
	if response := request(restarted, http.MethodGet, "/api/v1/status", "", cookie, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("successfully locked cookie returned after restart: %d", response.Code)
	}
}

func TestLockReportsPersistentFailureWhileRevokingCurrentProcess(t *testing.T) {
	persistence := &fakeBrowserSessionPersistence{}
	api := newPersistentTestAPI(t, time.Now, &fakeBackend{}, persistence)
	cookie, csrf := establish(t, api)
	session, ok := api.sessions.valid(cookie.Value)
	if !ok {
		t.Fatal("initial session is not active")
	}
	persistence.mu.Lock()
	persistence.deleteErr = errors.New("private database failure detail")
	persistence.mu.Unlock()

	locked := requestWithCSRF(api, http.MethodPost, "/api/v1/access/logout", `{}`, cookie, csrf)
	assertRevocationPending(t, locked)
	if strings.Contains(locked.Body.String(), "private database failure detail") {
		t.Fatal("raw database failure leaked into the public response")
	}
	if response := request(api, http.MethodGet, "/api/v1/status", "", cookie, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("failed persistence left the current process authenticated: %d", response.Code)
	}
	select {
	case <-session.done:
	default:
		t.Fatal("failed persistence left session event streams active")
	}
	persistence.mu.Lock()
	calls := persistence.deleteCalls
	_, remains := persistence.sessions[sessionKey(cookie.Value)]
	persistence.mu.Unlock()
	if calls != 3 || !remains {
		t.Fatalf("persistent failure attempts = %d, row remains = %v", calls, remains)
	}
	api.sessions.mu.Lock()
	_, queued := api.sessions.undeleted[sessionKey(cookie.Value)]
	api.sessions.mu.Unlock()
	if !queued {
		t.Fatal("failed durable lock lost its retry record")
	}
}

func TestLaterLockCannotHideAnEarlierPendingRevocation(t *testing.T) {
	persistence := &fakeBrowserSessionPersistence{}
	backend := &fakeBackend{}
	api := newPersistentTestAPI(t, time.Now, backend, persistence)
	firstCookie, firstCSRF := establish(t, api)
	persistence.mu.Lock()
	persistence.deleteErrors = map[string]error{sessionKey(firstCookie.Value): errors.New("first session delete unavailable")}
	persistence.mu.Unlock()
	assertRevocationPending(t, requestWithCSRF(api, http.MethodPost, "/api/v1/access/logout", `{}`, firstCookie, firstCSRF))

	// Creating a new session retries pending deletions, but a remaining failure
	// must still be reported by its later lock even if that new row is deleted.
	secondCookie, secondSession, err := api.sessions.create()
	if err != nil {
		t.Fatal(err)
	}
	second := &http.Cookie{Name: sessionCookieName, Value: secondCookie}
	assertRevocationPending(t, requestWithCSRF(api, http.MethodPost, "/api/v1/access/logout", `{}`, second, secondSession.csrf))
	persistence.mu.Lock()
	persistence.deleteErrors = nil
	persistence.mu.Unlock()
	thirdCookie, thirdSession, err := api.sessions.create()
	if err != nil {
		t.Fatal(err)
	}
	third := &http.Cookie{Name: sessionCookieName, Value: thirdCookie}
	if locked := requestWithCSRF(api, http.MethodPost, "/api/v1/access/logout", `{}`, third, thirdSession.csrf); locked.Code != http.StatusNoContent {
		t.Fatalf("recovered storage lock = %d: %s", locked.Code, locked.Body.String())
	}
	restarted := newPersistentTestAPI(t, time.Now, backend, persistence)
	for _, cookie := range []*http.Cookie{firstCookie, second, third} {
		if response := request(restarted, http.MethodGet, "/api/v1/status", "", cookie, ""); response.Code != http.StatusUnauthorized {
			t.Fatalf("old cookie returned after a successful recovery lock: %d", response.Code)
		}
	}
}

type notifyingSessionPersistence struct {
	*fakeBrowserSessionPersistence
	deleted chan struct{}
}

func (p *notifyingSessionPersistence) DeleteBrowserSession(token string) error {
	err := p.fakeBrowserSessionPersistence.DeleteBrowserSession(token)
	p.deleted <- struct{}{}
	return err
}

func TestShutdownCancelsRevocationBackoffWithoutDetachedRetries(t *testing.T) {
	persistence := &notifyingSessionPersistence{
		fakeBrowserSessionPersistence: &fakeBrowserSessionPersistence{},
		deleted:                       make(chan struct{}, 8),
	}
	api := newPersistentTestAPI(t, time.Now, &fakeBackend{}, persistence)
	cookie, csrf := establish(t, api)
	persistence.mu.Lock()
	persistence.deleteErr = errors.New("delete unavailable")
	persistence.mu.Unlock()
	req := httptest.NewRequest(http.MethodPost, testOrigin+"/api/v1/access/logout", strings.NewReader(`{}`)).WithContext(api.requests)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, csrf)
	req.AddCookie(cookie)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		api.ServeHTTP(response, req)
		close(done)
	}()
	select {
	case <-persistence.deleted:
	case <-time.After(time.Second):
		t.Fatal("lock did not try durable deletion")
	}
	api.CancelRequests()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not stop revocation backoff")
	}
	assertRevocationPending(t, response)
	select {
	case <-persistence.deleted:
		t.Fatal("durable retry continued after request shutdown")
	case <-time.After(250 * time.Millisecond):
	}
}
