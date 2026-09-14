package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewMetricsMuxPprofDisabled asserts that the profiling endpoints are not reachable
// on the metrics mux unless they were explicitly enabled.
func TestNewMetricsMuxPprofDisabled(t *testing.T) {
	mux := newMetricsMux(PrometheusHTTPServerConfig{Addr: ":2112"})

	paths := []string{
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/cmdline",
		"/debug/pprof/profile",
		"/debug/pprof/symbol",
		"/debug/pprof/trace",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))

			// The catch-all "/" handler must not serve profiling data either, so anything
			// other than the index HTML is a leak. The mux falls through to "/" here.
			if strings.Contains(w.Body.String(), "Types of profiles available") {
				t.Fatalf("%s served the pprof index while pprof was disabled", path)
			}
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "kube-vip Metrics") {
				t.Fatalf("%s = status %d body %q, want the index page from the catch-all handler",
					path, w.Code, w.Body.String())
			}
		})
	}
}

// TestNewMetricsMuxPprofEnabled asserts the profiling endpoints are wired up when enabled.
func TestNewMetricsMuxPprofEnabled(t *testing.T) {
	mux := newMetricsMux(PrometheusHTTPServerConfig{Addr: ":2112", EnablePprof: true})

	tests := []struct {
		path     string
		wantBody string
	}{
		{path: "/debug/pprof/", wantBody: "Types of profiles available"},
		// debug=1 selects the legacy text format; the default is binary protobuf.
		{path: "/debug/pprof/heap?debug=1", wantBody: "heap profile:"},
		{path: "/debug/pprof/goroutine?debug=1", wantBody: "goroutine profile: total"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("%s status = %d, want %d", tt.path, w.Code, http.StatusOK)
			}
			if !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Fatalf("%s body missing %q, got %q", tt.path, tt.wantBody, w.Body.String())
			}
		})
	}

	// The remaining handlers are asserted by registration rather than by invocation:
	// /debug/pprof/profile and /trace block for seconds by design, and /cmdline output
	// depends on the test binary's argv.
	for pattern, want := range map[string]string{
		"/debug/pprof/cmdline": "/debug/pprof/cmdline",
		"/debug/pprof/profile": "/debug/pprof/profile",
		"/debug/pprof/symbol":  "/debug/pprof/symbol",
		"/debug/pprof/trace":   "/debug/pprof/trace",
	} {
		req := httptest.NewRequest(http.MethodGet, pattern, nil)
		if _, got := mux.Handler(req); got != want {
			t.Errorf("handler pattern for %s = %q, want %q", pattern, got, want)
		}
	}
}

// TestNewMetricsMuxDoesNotDelegateToDefaultServeMux is a regression test for profiling
// endpoints leaking through http.DefaultServeMux. Importing net/http/pprof registers
// /debug/pprof/* on the default mux from init, i.e. before any flag is parsed, so the
// metrics mux must never delegate to it.
func TestNewMetricsMuxDoesNotDelegateToDefaultServeMux(t *testing.T) {
	// Establish the hazard: the default mux really does have pprof registered even though
	// nothing in this test enabled it.
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	if _, pattern := http.DefaultServeMux.Handler(req); !strings.Contains(pattern, "/debug/pprof/") {
		t.Skipf("net/http/pprof no longer registers on DefaultServeMux (pattern %q), "+
			"regression test no longer meaningful", pattern)
	}

	// With pprof disabled the metrics mux must resolve /debug/pprof/ to its own catch-all,
	// not to the default mux's pprof index.
	mux := newMetricsMux(PrometheusHTTPServerConfig{Addr: ":2112"})
	_, pattern := mux.Handler(req)
	if pattern != "/" {
		t.Fatalf("pattern for /debug/pprof/ = %q, want %q (the catch-all handler)", pattern, "/")
	}
}

// TestNewMetricsMuxRootEndpoint covers the index page and its conditional pprof link.
func TestNewMetricsMuxRootEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		enablePprof bool
		wantBody    []string
		notWantBody []string
	}{
		{
			name:        "without pprof",
			enablePprof: false,
			wantBody:    []string{"kube-vip Metrics", `<a href="/metrics">Metrics</a>`},
			notWantBody: []string{"/debug/pprof/"},
		},
		{
			name:        "with pprof enabled",
			enablePprof: true,
			wantBody: []string{
				"kube-vip Metrics",
				`<a href="/metrics">Metrics</a>`,
				`<a href="/debug/pprof/">pprof Profiler</a>`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := newMetricsMux(PrometheusHTTPServerConfig{
				Addr:        ":2112",
				EnablePprof: tt.enablePprof,
			})

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

			if w.Code != http.StatusOK {
				t.Fatalf("root status = %d, want %d", w.Code, http.StatusOK)
			}
			for _, want := range tt.wantBody {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("root body missing %q, got %q", want, w.Body.String())
				}
			}
			for _, notWant := range tt.notWantBody {
				if strings.Contains(w.Body.String(), notWant) {
					t.Errorf("root body unexpectedly contains %q, got %q", notWant, w.Body.String())
				}
			}
		})
	}
}

// TestNewMetricsMuxMetricsEndpoint asserts the Prometheus handler stays wired up in both modes.
func TestNewMetricsMuxMetricsEndpoint(t *testing.T) {
	for _, enablePprof := range []bool{false, true} {
		mux := newMetricsMux(PrometheusHTTPServerConfig{Addr: ":2112", EnablePprof: enablePprof})

		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

		if w.Code != http.StatusOK {
			t.Errorf("metrics status (enablePprof=%v) = %d, want %d", enablePprof, w.Code, http.StatusOK)
		}
		// Assert the Prometheus handler answered rather than the catch-all HTML handler,
		// without depending on which collectors happen to be in the default registry.
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
			t.Errorf("metrics Content-Type (enablePprof=%v) = %q, want text/plain", enablePprof, ct)
		}
		if strings.Contains(w.Body.String(), "kube-vip Metrics") {
			t.Errorf("metrics endpoint (enablePprof=%v) served the index HTML instead of metrics", enablePprof)
		}
	}
}
