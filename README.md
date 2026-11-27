# helm-exporter

[![CI](https://github.com/sabzco/helm-exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/sabzco/helm-exporter/actions/workflows/ci.yml)
[![Docker Hub](https://img.shields.io/docker/v/sabzco/helm-exporter?sort=semver&label=docker)](https://hub.docker.com/r/sabzco/helm-exporter)

A Prometheus exporter for Helm v3 releases that is driven by a Kubernetes watch.
Its metrics are compatible with
[sstarcher/helm-exporter](https://github.com/sstarcher/helm-exporter).

## Why

sstarcher/helm-exporter runs Helm's `action.List` on every cycle (and on every
scrape if `-interval-duration` is unset). Each run LISTs every `owner=helm`
Secret in every namespace and decodes every revision, so load on the API
server grows with (releases × history × scrape rate).

This exporter does one paged LIST at startup, then keeps a WATCH open:

- It runs one shared informer on `core/v1` Secrets with
  `labelSelector=owner=helm` and `fieldSelector=type=helm.sh/release.v1`.
- A `SetTransform` hook decodes each Secret once, when it arrives, into a small
  struct: release, namespace, revision, status, chart, chart version, app
  version, last deployed and description. `.data` and the rest of the object
  are dropped, so the cache holds a few hundred bytes per revision instead of
  the full release payload.
- A custom `prometheus.Collector` reads the cache at scrape time. It keeps the
  highest revision per (namespace, release) and ignores superseded ones. When
  a release is uninstalled its Secrets are deleted, so its series disappear
  without any gauge cleanup.

A full re-LIST happens only when the watch can't resume (for example, after
"resource version too old"). There is no periodic resync.

On a cluster with 413 releases (1193 revision Secrets), the initial sync
took about 28s and the Go heap was about 7 MB afterwards.

## Flags

| Flag | Default | Description |
|---|---|---|
| `-listen-address` | `:9571` | Address to serve `/metrics`, `/healthz` and `/readyz` on. |
| `-kubeconfig` | `""` | Path to a kubeconfig. Empty means in-cluster config, or `$KUBECONFIG`. |
| `-namespaces` | `""` | Comma-separated namespaces to watch. Empty means all namespaces. When set, one informer runs per namespace, so a namespaced Role is enough. |
| `-namespaces-ignore` | `""` | Comma-separated namespaces to skip, used only when `-namespaces` is empty. They are filtered on the server side with `metadata.namespace!=` field selectors. |
| `-info-metric` | `true` | Export `helm_chart_info`. |
| `-timestamp-metric` | `true` | Export `helm_chart_timestamp`. |
| `-status-in-metric` | `false` | Add a `status` label to `helm_chart_info`. |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error`. |
| `-version` | | Print the version and exit. |

The flags `-config`, `-interval-duration`, `-latest-chart-version`,
`-outdated-metric` and `-history-metric` are accepted and ignored, with a
warning in the log. This lets the image run under the upstream
sstarcher/helm-exporter chart without changing its arguments.

## Endpoints

- `/metrics`: Prometheus metrics.
- `/healthz`: always `200` while the process is serving.
- `/readyz`: `200` once every informer has finished its initial sync (`HasSynced`), otherwise `503`.

## Metrics

| Metric | Labels | Value |
|---|---|---|
| `helm_chart_info` | `chart`, `release`, `version`, `appVersion`, `revision`, `updated`, `namespace`, `description` (+ `status` with `-status-in-metric`) | Status code of the latest revision (see below). |
| `helm_chart_timestamp` | `chart`, `release`, `version`, `appVersion`, `updated`, `namespace` | Last deployed time, in Unix milliseconds. |
| `helm_exporter_informer_synced` | none | `1` once the cache has synced. Until then no release metrics are exported, to avoid flapping from a partial cache. |
| `helm_exporter_releases` | none | Number of releases (latest revisions) in the cache. |
| `helm_exporter_decode_errors_total` | none | Release Secrets that could not be decoded. Undecodable Secrets are skipped. |

Plus the standard `go_*` and `process_*` metrics.

The status codes are the same as upstream:

| Status | Code |
|---|---|
| `unknown` | 0 |
| `deployed` | 1 |
| `uninstalled` | 2 |
| `superseded` | 3 |
| `failed` | -1 |
| `uninstalling` | 5 |
| `pending-install` | 6 |
| `pending-upgrade` | 7 |
| `pending-rollback` | 8 |

The `updated` label has the same second-precision millisecond value as
upstream, so series identity is unchanged when you switch exporters.

### Differences from sstarcher/helm-exporter

- There is no `latestVersion` label. Upstream exports it as `""` when
  `-latest-chart-version=false`, and Prometheus treats an empty label as
  absent, so existing selectors and series still match.
- These are not implemented: `helm_chart_outdated`, `helm_release_history`,
  and the lookup of the latest chart version from registries (`-config`,
  `-latest-chart-version`). Registry lookups are periodic outbound calls, which
  doesn't fit an event-driven design. If needed, they could run as a separate
  low-frequency loop over the distinct chart names in the cache.
- A release's namespace is taken from the Secret's namespace. The Helm Secret
  driver always stores a release in its own namespace.
- Only the Helm v3 Secret storage driver is supported. ConfigMap and SQL
  storage are not.

## RBAC

To watch all namespaces, use a ClusterRole with `list` and `watch` on Secrets.
`get` is not needed:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: helm-exporter
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["list", "watch"]
```

With `-namespaces=a,b`, the same rule in a Role in each of those namespaces is
enough. The exporter doesn't need to read Namespaces.

The exporter does read Helm release payloads, which contain rendered manifests
and values, possibly including secrets. It keeps only the fields listed above.

## Install

With the Helm chart (published as an OCI artifact on GitHub Container Registry):

```bash
helm install helm-exporter oci://ghcr.io/sabzco/charts/helm-exporter \
  --namespace monitoring --set serviceMonitor.create=true
```

The chart creates a Deployment, Service, ServiceAccount, optional
ServiceMonitor, and a ClusterRole, or one Role per namespace when `namespaces`
is set. See [`charts/helm-exporter/values.yaml`](charts/helm-exporter/values.yaml)
for all values.

Or run the image directly with a kubeconfig:

```bash
docker run --rm -p 9571:9571 -v ~/.kube/config:/kubeconfig:ro \
  sabzco/helm-exporter -kubeconfig /kubeconfig
```

Images are published to Docker Hub as
[`sabzco/helm-exporter`](https://hub.docker.com/r/sabzco/helm-exporter) for
`linux/amd64` and `linux/arm64`. They're based on distroless
`static-debian12:nonroot` and run as UID 65532.

## Build and test

```bash
go test -race ./...
docker build -t helm-exporter .
```

To build behind a mirror, pass `--build-arg BUILD_IMAGE=...`,
`--build-arg RUNTIME_IMAGE=...` and `--build-arg GOPROXY=...`.

Tests use a fake clientset. They cover payload decoding (gzip and the legacy
plain form) and verify that the latest revision wins, that deleting an old
revision changes nothing, that an uninstalled release disappears, and that the
namespace allow list works.

## Release

Push a tag such as `v0.2.0`. The [release workflow](.github/workflows/release.yml)
runs the tests, then publishes:

- the image `sabzco/helm-exporter:0.2.0` (plus `0.2` and `latest`),
- the chart `oci://ghcr.io/sabzco/charts/helm-exporter:0.2.0`, with chart
  `version` and `appVersion` both set from the tag,
- a GitHub release with generated notes and the packaged chart attached.

## License

[Apache License 2.0](LICENSE)
