package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// statusCodes is the same mapping sstarcher/helm-exporter uses, so existing
// alerts such as `helm_chart_info < 0` (failed) keep working.
var statusCodes = map[string]float64{
	"unknown":          0,
	"deployed":         1,
	"uninstalled":      2,
	"superseded":       3,
	"failed":           -1,
	"uninstalling":     5,
	"pending-install":  6,
	"pending-upgrade":  7,
	"pending-rollback": 8,
}

func statusCode(s string) (float64, string) {
	if v, ok := statusCodes[s]; ok {
		return v, s
	}
	return 0, "unknown"
}

// watcher runs one Secret informer per watched namespace (or a single
// cluster-wide one) and keeps only a decoded summary of each Secret.
type watcher struct {
	factories []informers.SharedInformerFactory
	informers []cache.SharedIndexInformer
}

func newWatcher(cs kubernetes.Interface, namespaces, ignore []string, decodeErrors prometheus.Counter) (*watcher, error) {
	fieldSelector := "type=" + string(helmSecretType)
	if len(namespaces) == 0 {
		// Cluster-wide: exclude ignored namespaces server side.
		for _, ns := range ignore {
			fieldSelector += ",metadata.namespace!=" + ns
		}
		namespaces = []string{metav1.NamespaceAll}
	}

	w := &watcher{}
	for _, ns := range namespaces {
		f := informers.NewSharedInformerFactoryWithOptions(cs, 0,
			informers.WithNamespace(ns),
			informers.WithTweakListOptions(func(o *metav1.ListOptions) {
				o.LabelSelector = "owner=helm"
				o.FieldSelector = fieldSelector
			}),
		)
		inf := f.Core().V1().Secrets().Informer()
		if err := inf.SetTransform(transform(decodeErrors)); err != nil {
			return nil, fmt.Errorf("set transform: %w", err)
		}
		w.factories = append(w.factories, f)
		w.informers = append(w.informers, inf)
	}
	return w, nil
}

// transform replaces each Secret with a small helmRelease, dropping .data,
// managedFields and everything else we do not need.
func transform(decodeErrors prometheus.Counter) cache.TransformFunc {
	return func(obj interface{}) (interface{}, error) {
		s, ok := obj.(*corev1.Secret)
		if !ok {
			// Already transformed or a tombstone: leave as is.
			return obj, nil
		}
		r := releaseFromSecret(s)
		if r.DecodeErr != nil {
			decodeErrors.Inc()
			slog.Warn("cannot decode helm release secret", "namespace", s.Namespace, "name", s.Name, "err", r.DecodeErr)
		}
		return r, nil
	}
}

func (w *watcher) Start(ctx context.Context) {
	for _, f := range w.factories {
		f.Start(ctx.Done())
	}
}

func (w *watcher) HasSynced() bool {
	for _, inf := range w.informers {
		if !inf.HasSynced() {
			return false
		}
	}
	return true
}

// latest returns the highest revision of every release, keyed by
// namespace/release. Superseded revisions are skipped; once all of a
// release's Secrets are deleted, the release simply disappears.
func (w *watcher) latest() map[string]*helmRelease {
	out := map[string]*helmRelease{}
	for _, inf := range w.informers {
		for _, obj := range inf.GetStore().List() {
			r, ok := obj.(*helmRelease)
			if !ok || r.DecodeErr != nil {
				continue
			}
			key := r.Namespace + "/" + r.Release
			if cur, ok := out[key]; !ok || r.Revision > cur.Revision {
				out[key] = r
			}
		}
	}
	return out
}

type collectorOptions struct {
	infoMetric      bool
	timestampMetric bool
	statusInMetric  bool
}

// collector builds metrics from the informer cache at scrape time.
type collector struct {
	w    *watcher
	opts collectorOptions

	infoDesc, timestampDesc, syncedDesc, releasesDesc *prometheus.Desc
}

func newCollector(w *watcher, opts collectorOptions) *collector {
	infoLabels := []string{"chart", "release", "version", "appVersion", "revision", "updated", "namespace", "description"}
	if opts.statusInMetric {
		infoLabels = append(infoLabels, "status")
	}
	return &collector{
		w:    w,
		opts: opts,
		infoDesc: prometheus.NewDesc("helm_chart_info",
			"Information on helm releases; value is the release status code.", infoLabels, nil),
		timestampDesc: prometheus.NewDesc("helm_chart_timestamp",
			"Timestamps of helm releases (last deployed, unix milliseconds).",
			[]string{"chart", "release", "version", "appVersion", "updated", "namespace"}, nil),
		syncedDesc: prometheus.NewDesc("helm_exporter_informer_synced",
			"1 when the Secret informer cache has completed its initial sync.", nil, nil),
		releasesDesc: prometheus.NewDesc("helm_exporter_releases",
			"Number of Helm releases (latest revisions) in the cache.", nil, nil),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.infoDesc
	ch <- c.timestampDesc
	ch <- c.syncedDesc
	ch <- c.releasesDesc
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	synced := c.w.HasSynced()
	ch <- prometheus.MustNewConstMetric(c.syncedDesc, prometheus.GaugeValue, b2f(synced))
	if !synced {
		// A partial cache would make releases flap; export nothing until synced.
		return
	}

	releases := c.w.latest()
	ch <- prometheus.MustNewConstMetric(c.releasesDesc, prometheus.GaugeValue, float64(len(releases)))
	for _, r := range releases {
		code, status := statusCode(strings.ToLower(r.Status))
		// Second precision in milliseconds, exactly like upstream, so the
		// "updated" label value does not change when switching exporters.
		updatedMs := r.LastDeployed.Unix() * 1000
		updated := strconv.FormatInt(updatedMs, 10)

		if c.opts.infoMetric {
			lv := []string{r.Chart, r.Release, r.ChartVersion, r.AppVersion,
				strconv.Itoa(r.Revision), updated, r.Namespace, r.Description}
			if c.opts.statusInMetric {
				lv = append(lv, status)
			}
			ch <- prometheus.MustNewConstMetric(c.infoDesc, prometheus.GaugeValue, code, lv...)
		}
		if c.opts.timestampMetric {
			ch <- prometheus.MustNewConstMetric(c.timestampDesc, prometheus.GaugeValue, float64(updatedMs),
				r.Chart, r.Release, r.ChartVersion, r.AppVersion, updated, r.Namespace)
		}
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
