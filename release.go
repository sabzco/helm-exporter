package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// helmSecretType is the Secret type Helm v3's Secret storage driver writes.
const helmSecretType corev1.SecretType = "helm.sh/release.v1"

var magicGzip = []byte{0x1f, 0x8b, 0x08}

// helmRelease is the small object kept in the informer cache instead of the
// full Secret. It embeds ObjectMeta (only namespace/name/resourceVersion are
// kept) so the informer's key functions keep working.
type helmRelease struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Release      string
	Revision     int
	Status       string
	Chart        string
	ChartVersion string
	AppVersion   string
	Description  string
	LastDeployed time.Time

	// DecodeErr is set when the Secret could not be decoded. Such entries
	// stay in the cache (so the key is tracked) but are not exported.
	DecodeErr error
}

func (r *helmRelease) DeepCopyObject() runtime.Object {
	c := *r
	r.ObjectMeta.DeepCopyInto(&c.ObjectMeta)
	return &c
}

// releasePayload is the subset of helm.sh/helm/v3/pkg/release.Release that we
// need. Keeping it local avoids pulling in the whole Helm module.
type releasePayload struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Info      *struct {
		LastDeployed time.Time `json:"last_deployed"`
		Description  string    `json:"description"`
		Status       string    `json:"status"`
	} `json:"info"`
	Chart *struct {
		Metadata *struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
}

// decodeReleasePayload mirrors Helm's driver.decodeRelease: the Secret's
// "release" value is base64 text of (optionally gzipped) release JSON.
func decodeReleasePayload(data []byte) (*releasePayload, error) {
	b := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	n, err := base64.StdEncoding.Decode(b, data)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	b = b[:n]

	if len(b) > 3 && bytes.Equal(b[:3], magicGzip) {
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer r.Close()
		if b, err = io.ReadAll(r); err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
	}

	var p releasePayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return &p, nil
}

// releaseFromSecret decodes a Helm release Secret into a helmRelease. It never
// returns nil; decoding problems are reported in DecodeErr.
func releaseFromSecret(s *corev1.Secret) *helmRelease {
	r := &helmRelease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       s.Namespace,
			Name:            s.Name,
			UID:             s.UID,
			ResourceVersion: s.ResourceVersion,
		},
	}
	if s.Type != helmSecretType {
		r.DecodeErr = fmt.Errorf("unexpected secret type %q", s.Type)
		return r
	}
	data, ok := s.Data["release"]
	if !ok {
		r.DecodeErr = fmt.Errorf("no release key")
		return r
	}
	p, err := decodeReleasePayload(data)
	if err != nil {
		r.DecodeErr = err
		return r
	}

	r.Release = p.Name
	r.Revision = p.Version
	if p.Info != nil {
		r.Status = p.Info.Status
		r.Description = p.Info.Description
		r.LastDeployed = p.Info.LastDeployed
	}
	if p.Chart != nil && p.Chart.Metadata != nil {
		r.Chart = p.Chart.Metadata.Name
		r.ChartVersion = p.Chart.Metadata.Version
		r.AppVersion = p.Chart.Metadata.AppVersion
	}
	if r.Release == "" {
		r.DecodeErr = fmt.Errorf("release payload has no name")
	}
	return r
}
