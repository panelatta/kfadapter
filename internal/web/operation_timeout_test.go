package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type operationBudgetBackend struct {
	fakeBackend
	work      func(context.Context) error
	succeeded atomic.Bool
}

func (b *operationBudgetBackend) Login(ctx context.Context, in LoginInput) (Account, error) {
	if err := b.work(ctx); err != nil {
		return Account{}, err
	}
	b.succeeded.Store(true)
	return Account{Provider: in.Provider, Display: in.Account}, nil
}
func (b *operationBudgetBackend) Refresh(ctx context.Context, _ string) error {
	if err := b.work(ctx); err != nil {
		return err
	}
	b.succeeded.Store(true)
	return nil
}

func startOperationBudgetServer(t *testing.T, config Config, deps Dependencies, configure func(*http.Server)) (*http.Server, *API, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config.Listen = listener.Addr().String()
	server, api, err := NewHTTPServer(config, deps)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	if configure != nil {
		configure(server)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { api.CancelRequests(); _ = server.Close(); _ = listener.Close() })
	return server, api, "http://" + listener.Addr().String()
}

func budgetRequest(t *testing.T, api *API, origin, path string) *http.Request {
	t.Helper()
	body := `{}`
	if path == "/api/v1/auth/login" {
		body = `{"provider":"kuaifan","account":"review@example.com","password":"test-only"}`
	}
	request, err := http.NewRequest(http.MethodPost, origin+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	token, session, err := api.sessions.create()
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	request.Header.Set("Origin", origin)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(csrfHeaderName, session.csrf)
	return request
}

func TestHTTPOperationBudgetAllowsSlowSuccessfulBackend(t *testing.T) {
	for _, test := range []struct {
		path   string
		status int
	}{{"/api/v1/auth/login", http.StatusOK}, {"/api/v1/control/refresh", http.StatusAccepted}} {
		t.Run(test.path, func(t *testing.T) {
			backend := &operationBudgetBackend{work: func(ctx context.Context) error {
				timer := time.NewTimer(120 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
					return ctx.Err()
				case <-ctx.Done():
					return ctx.Err()
				}
			}}
			_, api, origin := startOperationBudgetServer(t, Config{WriteTimeout: 30 * time.Millisecond, OperationTimeout: 2 * time.Second}, Dependencies{Backend: backend}, nil)
			response, err := (&http.Client{Timeout: time.Second}).Do(budgetRequest(t, api, origin, test.path))
			if err != nil {
				t.Fatalf("successful backend response lost: %v", err)
			}
			defer response.Body.Close()
			if _, err := io.ReadAll(response.Body); err != nil {
				t.Fatalf("response body: %v", err)
			}
			if response.StatusCode != test.status || !backend.succeeded.Load() {
				t.Fatalf("status=%d backend success=%v", response.StatusCode, backend.succeeded.Load())
			}
		})
	}
}

func TestHTTPOperationBudgetCancelsAndRespondsBeforeLateBackendReturns(t *testing.T) {
	cancelled := make(chan error, 1)
	release := make(chan struct{})
	finished := make(chan struct{})
	backend := &operationBudgetBackend{work: func(ctx context.Context) error {
		defer close(finished)
		<-ctx.Done()
		cancelled <- ctx.Err()
		<-release
		return ctx.Err()
	}}
	defer func() {
		close(release)
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("late backend did not exit after release")
		}
	}()
	_, api, origin := startOperationBudgetServer(t, Config{WriteTimeout: 100 * time.Millisecond, OperationTimeout: 40 * time.Millisecond}, Dependencies{Backend: backend}, nil)
	response, err := (&http.Client{Timeout: time.Second}).Do(budgetRequest(t, api, origin, "/api/v1/auth/login"))
	if err != nil {
		t.Fatalf("timeout response: %v", err)
	}
	defer response.Body.Close()
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || problem.Code != "operation_timeout" {
		t.Fatalf("timeout response = %d, %s", response.StatusCode, problem.Code)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("timeout headers: %v", response.Header)
	}
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("backend context was not cancelled")
	}
	select {
	case <-finished:
		t.Fatal("fixture backend returned before release")
	default:
	}
	if backend.succeeded.Load() {
		t.Fatal("cancelled backend reported success")
	}
}

func TestHTTPOperationBudgetKeepsShutdownCancellation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	backend := &operationBudgetBackend{work: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		cancelled <- ctx.Err()
		return ctx.Err()
	}}
	server, api, origin := startOperationBudgetServer(t, Config{OperationTimeout: 2 * time.Second}, Dependencies{Backend: backend}, nil)
	finished := make(chan struct{})
	request := budgetRequest(t, api, origin, "/api/v1/auth/login")
	go func() {
		defer close(finished)
		response, _ := (&http.Client{Timeout: time.Second}).Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("backend did not start")
	}
	api.CancelRequests()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel backend")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("request did not finish after shutdown")
	}
}

type largeBudgetSubscription struct {
	fakeSubscriptions
	written chan error
}

func (s *largeBudgetSubscription) ServeSubscription(w http.ResponseWriter, _ *http.Request, _, _ string) {
	w.Header().Set("Content-Type", "text/plain")
	_, err := io.WriteString(w, strings.Repeat("x", 8<<20))
	s.written <- err
}

func TestHTTPResponseWriteStillBoundsBlockedClient(t *testing.T) {
	closed := make(chan struct{})
	var once sync.Once
	subscriptions := &largeBudgetSubscription{written: make(chan error, 1)}
	_, _, origin := startOperationBudgetServer(t, Config{OperationTimeout: 5 * time.Second, WriteTimeout: 40 * time.Millisecond}, Dependencies{Subscriptions: subscriptions}, func(server *http.Server) {
		server.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				once.Do(func() { close(closed) })
			}
		}
	})
	address := strings.TrimPrefix(origin, "http://")
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(connection, "GET /sub/%s HTTP/1.1\r\nHost: %s\r\n\r\n", strings.Repeat("A", 43), address); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-subscriptions.written:
		var networkError net.Error
		if !errors.As(err, &networkError) || !networkError.Timeout() {
			t.Fatalf("subscription Write returned %v; want a real network timeout, not a buffered success", err)
		}
	case <-time.After(time.Second):
		t.Fatal("subscription write outlived its network deadline")
	}
	// The client deliberately never reads the response; the five-second backend
	// budget must not replace the much shorter actual response-write budget.
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("blocked response write outlived its deadline")
	}
}

func TestHTTPEventStreamOutlivesOperationAndWriteBudgets(t *testing.T) {
	backend := &fakeBackend{}
	_, api, origin := startOperationBudgetServer(t, Config{OperationTimeout: 30 * time.Millisecond, WriteTimeout: 20 * time.Millisecond, SSEHeartbeat: 90 * time.Millisecond}, Dependencies{Backend: backend}, nil)
	request := budgetRequest(t, api, origin, "/api/v1/events")
	request.Method, request.Body, request.ContentLength = http.MethodGet, nil, 0
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE status %d", response.StatusCode)
	}
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("SSE stopped before heartbeat: %v", err)
		}
		if strings.Contains(line, ": keepalive") {
			break
		}
	}
	api.CancelRequests()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("SSE shutdown: %v", err)
	}
}

func TestHTTPOperationBudgetRetainsReadDeadline(t *testing.T) {
	_, _, origin := startOperationBudgetServer(t, Config{ReadTimeout: 30 * time.Millisecond, WriteTimeout: 100 * time.Millisecond, OperationTimeout: time.Second}, Dependencies{Backend: &fakeBackend{}}, nil)
	address := strings.TrimPrefix(origin, "http://")
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(connection, "POST /api/v1/access/setup HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nContent-Type: application/json\r\nContent-Length: 40\r\n\r\n{", address, origin); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// A read timeout also cancels net/http's request context. Depending on which
	// completion wins, either the API's bad-body response or cancellation's 503
	// is sent, but neither waits for the one-second operation budget.
	if response.StatusCode != http.StatusBadRequest && response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("slow body = %d, want read deadline rejection", response.StatusCode)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("read deadline was replaced by operation timeout: %v", elapsed)
	}
}

func TestHTTPOperationTimeoutConfiguration(t *testing.T) {
	for _, timeout := range []time.Duration{-time.Second, 5*time.Minute + time.Nanosecond} {
		if _, err := NewAPI(Config{Listen: testHost, OperationTimeout: timeout}, Dependencies{}); err == nil {
			t.Fatalf("accepted invalid operation timeout %v", timeout)
		}
	}
	api, err := NewAPI(Config{Listen: testHost}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if api.config.OperationTimeout != 3*time.Minute {
		t.Fatalf("default operation timeout = %v", api.config.OperationTimeout)
	}
}

func TestHTTPHashedAssetKeepsImmutableCacheHeaders(t *testing.T) {
	entries, err := fs.ReadDir(staticFiles, "static/dist/assets")
	if err != nil || len(entries) == 0 {
		t.Skip("frontend build output is not embedded")
	}
	_, _, origin := startOperationBudgetServer(t, Config{}, Dependencies{}, nil)
	response, err := (&http.Client{Timeout: time.Second}).Get(origin + "/assets/" + entries[0].Name())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Cache-Control"), "immutable") || response.Header.Get("Pragma") != "" {
		t.Fatalf("asset response = %d, %v", response.StatusCode, response.Header)
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatal("asset inherited timeout content type")
	}
}
