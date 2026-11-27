package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// startWatcher starts a watcher on a fake clientset and waits for its sync.
func startWatcher(t *testing.T, cs *fake.Clientset, namespaces []string) *watcher {
	t.Helper()
	w, err := newWatcher(cs, namespaces, nil, prometheus.NewCounter(prometheus.CounterOpts{Name: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.Start(ctx)
	waitFor(t, "informer sync", w.HasSynced)
	return w
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func revisions(w *watcher) map[string]int {
	out := map[string]int{}
	for k, r := range w.latest() {
		out[k] = r.Revision
	}
	return out
}

func TestLatestRevisionWinsAndDeletedReleaseDisappears(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset(
		helmSecret("web", "apps", 1, "superseded", true),
		helmSecret("web", "apps", 2, "deployed", true),
		helmSecret("db", "data", 1, "failed", false),
		// Not a Helm release: no owner=helm label.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "apps"}, Data: map[string][]byte{"k": []byte("v")}},
	)
	w := startWatcher(t, cs, nil)
	c := newCollector(w, collectorOptions{infoMetric: true, timestampMetric: true, statusInMetric: true})

	if got := revisions(w); len(got) != 2 || got["apps/web"] != 2 || got["data/db"] != 1 {
		t.Fatalf("initial latest = %v", got)
	}

	// The cache must hold decoded summaries, not Secrets with payloads.
	for _, obj := range w.informers[0].GetStore().List() {
		if _, ok := obj.(*helmRelease); !ok {
			t.Fatalf("cache holds %T, want *helmRelease", obj)
		}
	}

	expected := `
# HELP helm_chart_info Information on helm releases; value is the release status code.
# TYPE helm_chart_info gauge
helm_chart_info{appVersion="2.0",chart="mychart",description="rev 1",namespace="data",release="db",revision="1",status="failed",updated="1793530800000",version="1.1.0"} -1
helm_chart_info{appVersion="2.0",chart="mychart",description="rev 2",namespace="apps",release="web",revision="2",status="deployed",updated="1793534400000",version="1.2.0"} 1
# HELP helm_exporter_informer_synced 1 when the Secret informer cache has completed its initial sync.
# TYPE helm_exporter_informer_synced gauge
helm_exporter_informer_synced 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "helm_chart_info", "helm_exporter_informer_synced"); err != nil {
		t.Fatal(err)
	}

	// Upgrade web to revision 3 (helm also marks rev 2 superseded).
	secrets := cs.CoreV1().Secrets("apps")
	if _, err := secrets.Create(ctx, helmSecret("web", "apps", 3, "deployed", true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Update(ctx, helmSecret("web", "apps", 2, "superseded", true), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "revision 3", func() bool { return revisions(w)["apps/web"] == 3 })

	// Rollback-style history pruning: deleting an old revision changes nothing.
	if err := secrets.Delete(ctx, "sh.helm.release.v1.web.v1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Uninstall db: all its Secrets go away, so must its metrics.
	if err := cs.CoreV1().Secrets("data").Delete(ctx, "sh.helm.release.v1.db.v1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "db removal", func() bool { _, ok := revisions(w)["data/db"]; return !ok })

	if got := revisions(w); len(got) != 1 || got["apps/web"] != 3 {
		t.Fatalf("final latest = %v", got)
	}
	if n := testutil.CollectAndCount(c, "helm_chart_info"); n != 1 {
		t.Fatalf("helm_chart_info series = %d, want 1", n)
	}
	if n := testutil.CollectAndCount(c, "helm_chart_timestamp"); n != 1 {
		t.Fatalf("helm_chart_timestamp series = %d, want 1", n)
	}
}

func TestNamespaceAllowList(t *testing.T) {
	cs := fake.NewClientset(
		helmSecret("web", "apps", 1, "deployed", true),
		helmSecret("db", "data", 1, "deployed", true),
		helmSecret("mon", "monitoring", 1, "deployed", true),
	)
	w := startWatcher(t, cs, []string{"apps", "data"})
	got := revisions(w)
	if len(got) != 2 || got["apps/web"] != 1 || got["data/db"] != 1 {
		t.Fatalf("latest = %v", got)
	}
}

func TestNotSyncedExportsNothing(t *testing.T) {
	w, err := newWatcher(fake.NewClientset(helmSecret("web", "apps", 1, "deployed", true)), nil, nil,
		prometheus.NewCounter(prometheus.CounterOpts{Name: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	// Not started, so not synced.
	c := newCollector(w, collectorOptions{infoMetric: true, timestampMetric: true})
	expected := `
# HELP helm_exporter_informer_synced 1 when the Secret informer cache has completed its initial sync.
# TYPE helm_exporter_informer_synced gauge
helm_exporter_informer_synced 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}
