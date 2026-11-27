package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var testDeployed = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

// releaseJSON builds a JSON document shaped like helm's release.Release,
// including fields we ignore (manifest, config, templates).
func releaseJSON(name, ns string, rev int, status string) []byte {
	b, _ := json.Marshal(map[string]any{
		"name":      name,
		"namespace": ns,
		"version":   rev,
		"manifest":  "---\nkind: ConfigMap\n",
		"config":    map[string]any{"replicas": 3},
		"info": map[string]any{
			"first_deployed": testDeployed,
			"last_deployed":  testDeployed.Add(time.Duration(rev) * time.Hour),
			"description":    fmt.Sprintf("rev %d", rev),
			"status":         status,
			"notes":          "thanks",
		},
		"chart": map[string]any{
			"metadata":  map[string]any{"name": "mychart", "version": fmt.Sprintf("1.%d.0", rev), "appVersion": "2.0"},
			"templates": []any{map[string]any{"name": "t.yaml", "data": "Zm9v"}},
		},
	})
	return b
}

// encode mirrors helm's driver.encodeRelease (gzip) or the legacy plain form.
func encode(raw []byte, gz bool) []byte {
	if gz {
		var buf bytes.Buffer
		w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = w.Write(raw)
		_ = w.Close()
		raw = buf.Bytes()
	}
	return []byte(base64.StdEncoding.EncodeToString(raw))
}

func helmSecret(name, ns string, rev int, status string, gz bool) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, rev),
			Namespace: ns,
			Labels:    map[string]string{"owner": "helm", "name": name, "status": status, "version": fmt.Sprint(rev)},
		},
		Type: helmSecretType,
		Data: map[string][]byte{"release": encode(releaseJSON(name, ns, rev, status), gz)},
	}
}

func TestReleaseFromSecret(t *testing.T) {
	for _, gz := range []bool{true, false} {
		t.Run(fmt.Sprintf("gzip=%v", gz), func(t *testing.T) {
			r := releaseFromSecret(helmSecret("web", "apps", 4, "deployed", gz))
			if r.DecodeErr != nil {
				t.Fatalf("decode: %v", r.DecodeErr)
			}
			want := helmRelease{
				Release: "web", Revision: 4, Status: "deployed", Chart: "mychart",
				ChartVersion: "1.4.0", AppVersion: "2.0", Description: "rev 4",
				LastDeployed: testDeployed.Add(4 * time.Hour),
			}
			got := *r
			got.ObjectMeta, got.DecodeErr = metav1.ObjectMeta{}, nil
			if !got.LastDeployed.Equal(want.LastDeployed) {
				t.Fatalf("LastDeployed = %v, want %v", got.LastDeployed, want.LastDeployed)
			}
			got.LastDeployed = want.LastDeployed
			if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
				t.Fatalf("got %+v\nwant %+v", got, want)
			}
			if r.Namespace != "apps" || r.Name != "sh.helm.release.v1.web.v4" {
				t.Fatalf("meta = %s/%s", r.Namespace, r.Name)
			}
		})
	}
}

func TestReleaseFromSecretErrors(t *testing.T) {
	bad := helmSecret("web", "apps", 1, "deployed", true)
	bad.Data["release"] = []byte("!!!not base64")
	wrongType := helmSecret("web", "apps", 1, "deployed", true)
	wrongType.Type = corev1.SecretTypeOpaque
	noKey := helmSecret("web", "apps", 1, "deployed", true)
	delete(noKey.Data, "release")
	badJSON := helmSecret("web", "apps", 1, "deployed", false)
	badJSON.Data["release"] = encode([]byte("{"), false)
	badGzip := helmSecret("web", "apps", 1, "deployed", false)
	badGzip.Data["release"] = encode([]byte{0x1f, 0x8b, 0x08, 0, 1, 2}, false)

	for name, s := range map[string]*corev1.Secret{
		"base64": bad, "type": wrongType, "missing key": noKey, "json": badJSON, "gzip": badGzip,
	} {
		if r := releaseFromSecret(s); r.DecodeErr == nil {
			t.Errorf("%s: expected decode error", name)
		}
	}
}
