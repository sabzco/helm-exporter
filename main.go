// Command helm-exporter exports Prometheus metrics for Helm v3 releases.
//
// Unlike sstarcher/helm-exporter, it does not periodically LIST every Helm
// release Secret. It keeps a Secret informer (one LIST, then WATCH) whose
// cache holds only a small decoded summary of each release, and builds
// metrics from that cache at scrape time.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		listenAddr      = flag.String("listen-address", ":9571", "Address to serve /metrics, /healthz and /readyz on.")
		kubeconfig      = flag.String("kubeconfig", "", "Path to a kubeconfig. Empty means in-cluster config (or $KUBECONFIG).")
		namespaces      = flag.String("namespaces", "", "Comma-separated namespaces to watch. Empty means all namespaces.")
		namespacesIgn   = flag.String("namespaces-ignore", "", "Comma-separated namespaces to ignore (only used when -namespaces is empty).")
		infoMetric      = flag.Bool("info-metric", true, "Export helm_chart_info.")
		timestampMetric = flag.Bool("timestamp-metric", true, "Export helm_chart_timestamp.")
		statusInMetric  = flag.Bool("status-in-metric", false, "Add a status label to helm_chart_info.")
		logLevel        = flag.String("log-level", "info", "Log level: debug, info, warn, error.")
		showVersion     = flag.Bool("version", false, "Print the version and exit.")
	)
	// Flags of sstarcher/helm-exporter that do not apply here. They are
	// accepted so this image can replace upstream's in its Helm chart.
	ignored := map[string]*string{}
	for _, name := range []string{"config", "interval-duration", "latest-chart-version", "outdated-metric", "history-metric"} {
		ignored[name] = flag.String(name, "", "Ignored; accepted for compatibility with sstarcher/helm-exporter.")
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(*logLevel)})))
	flag.Visit(func(f *flag.Flag) {
		if _, ok := ignored[f.Name]; ok {
			slog.Warn("flag is accepted for compatibility but has no effect", "flag", f.Name, "value", f.Value.String())
		}
	})

	if err := run(*listenAddr, *kubeconfig, splitList(*namespaces), splitList(*namespacesIgn), collectorOptions{
		infoMetric:      *infoMetric,
		timestampMetric: *timestampMetric,
		statusInMetric:  *statusInMetric,
	}); err != nil {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(listenAddr, kubeconfig string, namespaces, ignore []string, opts collectorOptions) error {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	cfg.UserAgent = "helm-exporter"
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	decodeErrors := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "helm_exporter_decode_errors_total",
		Help: "Helm release Secrets that could not be decoded.",
	})
	w, err := newWatcher(cs, namespaces, ignore, decodeErrors)
	if err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		decodeErrors,
		newCollector(w, opts),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("starting informers", "version", version, "namespaces", namespaces, "ignore", ignore)
	w.Start(ctx)
	go func() {
		start := time.Now()
		for !w.HasSynced() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
		slog.Info("informer cache synced", "took", time.Since(start).Round(time.Millisecond))
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(rw http.ResponseWriter, _ *http.Request) {
		if !w.HasSynced() {
			http.Error(rw, "informer cache not synced", http.StatusServiceUnavailable)
			return
		}
		_, _ = rw.Write([]byte("ok\n"))
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("listening", "address", listenAddr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug", "trace":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "fatal", "panic":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
