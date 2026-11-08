package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"net/http/pprof"

	"boot.dev/linko/internal/store"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type server struct {
	httpServer *http.Server
	store      store.Store
	logger     *slog.Logger
	cancel     context.CancelFunc
}

func newServer(store store.Store, port int, logger *slog.Logger, cancel context.CancelFunc) *server {
	mux := http.NewServeMux()
	h := otelhttp.NewHandler(mux, "linko")

	s := &server{
		store:  store,
		logger: logger,
		cancel: cancel,
	}
	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: metricsMiddleware(requestIdHandler()(requestLogger(logger)(h))),
	}

	mux.HandleFunc("GET /", s.handlerIndex)
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)

	mux.Handle("GET /debug/pprof/", s.authMiddleware(http.HandlerFunc(pprof.Index)))
	mux.Handle("GET /debug/pprof/profile", s.authMiddleware(http.HandlerFunc(pprof.Profile)))


	return s
}

func (s *server) start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}

	s.logger.Debug(fmt.Sprintf("Linko is running on http://localhost:%d", ln.Addr().(*net.TCPAddr).Port))

	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func (s *server) shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (r *spyReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n
	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

const LogContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Error    error
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logContext, ok := ctx.Value(LogContextKey).(*LogContext); ok {
		logContext.Error = err
	}

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError:
		http.Error(w, http.StatusText(status), status)
	default:
		http.Error(w, err.Error(), status)
	}
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			spyReader := &spyReadCloser{ReadCloser: r.Body}
			r.Body = spyReader
			spyWriter := &spyResponseWriter{ResponseWriter: w}

			logCtx := &LogContext{}
			r = r.WithContext(context.WithValue(r.Context(), LogContextKey, logCtx))

			next.ServeHTTP(spyWriter, r)

			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client_ip", redactIP(r.RemoteAddr)),
				slog.Duration("duration", time.Since(start)),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
				slog.String("request_id", w.Header().Get("X-Request-ID")),
			}
			if logCtx.Username != "" {
				attrs = append(attrs, slog.String("user", logCtx.Username))
			}
			if logCtx.Error != nil {
				attrs = append(attrs, slog.Any("error", logCtx.Error))
			}

			logger.Info("Served request", attrs...)
		})
	}
}

func redactIP(ip string) string {
	host, _, err := net.SplitHostPort(ip)
	if err != nil {
		return ip
	}
	parsedIP := net.ParseIP(host)
	if parsedIP == nil {
		return ip
	}
	if parsedIP.To4() == nil {
		return ip
	}
	// IPv4 address
	octets := strings.Split(parsedIP.String(), ".")
	return strings.Join(append(octets[:3], "x"), ".")
}

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	go s.cancel()
}
