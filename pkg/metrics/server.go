package metrics

import (
	"context"
	"net/http"
	"net/http/pprof" //nolint:gosec // G108: the default mux is never served, see NewMux
	"sync"
	"time"

	log "log/slog"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServerConfig defines the HTTP server that exposes the metrics endpoint and,
// optionally, the profiling endpoints.
type ServerConfig struct {
	// Addr sets the http server address used to expose the metric endpoint
	Addr string
	// EnablePprof enables pprof profiling endpoints (debug only)
	EnablePprof bool
}

// NewMux builds the handler served on the metrics server address.
//
// pprof handlers are registered on this mux directly rather than relying on
// http.DefaultServeMux: importing net/http/pprof registers /debug/pprof/* on
// DefaultServeMux from the package's init function, which happens at process start
// regardless of EnablePprof. Any server left with a nil Handler would therefore
// expose the profiling endpoints even when the flag is off.
func NewMux(config ServerConfig) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	if config.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		pprofHTML := ""
		if config.EnablePprof {
			pprofHTML = `<p><a href="/debug/pprof/">pprof Profiler</a></p>`
		}
		_, _ = w.Write([]byte(`<html>
			<head><title>kube-vip</title></head>
			<body>
			<h1>kube-vip Metrics</h1>
			<p><a href="` + "/metrics" + `">Metrics</a></p>
			` + pprofHTML + `
			</body>
			</html>`))
	})

	return mux
}

// Serve runs the metrics HTTP server until ctx is cancelled, then shuts it down
// gracefully. It blocks for the lifetime of the server.
func Serve(ctx context.Context, config ServerConfig) {
	// Register the collectors before the listener accepts connections, otherwise a scrape
	// racing with startup is answered without any kube_vip_* series. RegisterPrometheusMetrics
	// is idempotent, so callers that register earlier are unaffected.
	RegisterPrometheusMetrics()

	if config.EnablePprof {
		log.Warn("pprof profiling endpoints enabled at /debug/pprof/, this exposes runtime "+
			"internals and allows unauthenticated CPU profiling; do not enable in production",
			"addr", config.Addr)
	}

	srv := &http.Server{
		Addr:              config.Addr,
		Handler:           NewMux(config),
		ReadHeaderTimeout: 2 * time.Second,
	}

	wg := sync.WaitGroup{}

	wg.Go(func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("prometheus HTTP server", "err", err)
		}
	})

	log.Info("prometheus HTTP server started", "addr", config.Addr)

	<-ctx.Done()

	// shut down on an independent context, the caller's is already cancelled
	ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctxShutDown); err != nil {
		log.Error("shutting down prometheus HTTP server", "err", err)
	}

	wg.Wait()

	log.Info("prometheus HTTP server stopped")
}
