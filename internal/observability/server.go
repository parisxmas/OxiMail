package observability

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/parisxmas/OxiMail/internal/store"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Server exposes Prometheus metrics and health probes on a dedicated
// HTTP port — :9090 by convention. It is wired as a component, like the
// protocol servers.
type Server struct {
	addr  string
	store *store.Store
	srv   *http.Server
	stop  sync.Once
}

// New builds the observability server. The store is used by /readyz to
// confirm the data backend is reachable.
func New(addr string, st *store.Store) *Server {
	s := &Server{addr: addr, store: st}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", handleLive)
	mux.HandleFunc("/readyz", s.handleReady)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	return s
}

// handleLive is the liveness probe: the binary is up and serving HTTP.
// It does not check downstreams, so a temporary backend outage does not
// cause an orchestrator to restart the process.
func handleLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleReady is the readiness probe: the server is ready to take
// traffic only if the store is reachable. A trivial Find on `domains`
// (which exists from EnsureSchema) is the cheapest smoke test.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if _, err := s.store.ListDomains(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("store unreachable\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Start serves until ctx is cancelled, then shuts the server down.
func (s *Server) Start(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.srv.ListenAndServe() }()
	log.Printf("observability: listening on %s", s.addr)
	select {
	case <-ctx.Done():
		return s.Stop()
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Stop shuts the server down. Safe to call more than once.
func (s *Server) Stop() error {
	s.stop.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
		log.Print("observability: stopped")
	})
	return nil
}
