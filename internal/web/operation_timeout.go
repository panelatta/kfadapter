package web

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

const operationTimeoutBody = `{"code":"operation_timeout","status":503,"title":"Operation timed out"}` + "\n"

// httpHandler gives finite management API calls their own operation deadline,
// then bounds delivery of the completed response. TimeoutHandler cancels the
// backend context and prevents late writes over its timeout response. Other
// routes keep their direct writers: subscription capacity slots must remain
// held throughout network delivery, and SSE owns its streaming lifecycle.
func (a *API) httpHandler() http.Handler {
	bounded := http.TimeoutHandler(a, a.config.OperationTimeout, operationTimeoutBody)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/events" {
			a.ServeHTTP(w, r)
			return
		}
		if r.URL.Path != "/api/v1" && !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			// Restore the original short network deadline for routes that do not
			// perform a multi-stage provider operation, without buffering them.
			controller := http.NewResponseController(w)
			if err := controller.SetWriteDeadline(time.Now().Add(a.config.WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return
			}
			a.ServeHTTP(w, r)
			return
		}
		bounded.ServeHTTP(&responseDeadlineWriter{ResponseWriter: w, timeout: a.config.WriteTimeout}, r)
	})
}

type responseDeadlineWriter struct {
	http.ResponseWriter
	timeout time.Duration
	started bool
	err     error
}

func (w *responseDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseDeadlineWriter) start() {
	if w.started {
		return
	}
	w.started = true
	w.err = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.timeout))
	if errors.Is(w.err, http.ErrNotSupported) {
		w.err = nil
	}
}

func (w *responseDeadlineWriter) WriteHeader(status int) {
	w.start()
	if w.err == nil {
		// Every completed API response carries these security headers. If the
		// timeout won instead, supply its own safe headers without leaking
		// fallback values into a completed handler's response.
		if w.Header().Get("Content-Security-Policy") == "" {
			setSecurityHeaders(w.Header())
			setNoStore(w.Header())
			w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
		}
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *responseDeadlineWriter) Write(body []byte) (int, error) {
	w.start()
	if w.err != nil {
		return 0, w.err
	}
	return w.ResponseWriter.Write(body)
}
