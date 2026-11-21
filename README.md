# metrics-mux

Run KEDA, prometheus-adapter, a cloud adapter or any other external metrics
adapter in the same Kubernetes cluster. metrics-mux registers itself as the
`external.metrics.k8s.io` API and forwards every request to the adapter that
owns the metric.

```
HPA controller --> kube-apiserver --> APIService v1beta1.external.metrics.k8s.io
                                              |
                                         metrics-mux  (authn/authz, routing)
                                     +--------+-----------+
                                     |                    |
                             KEDA metrics server   prometheus-adapter   ...
```

## Why

Kubernetes routes an API group to exactly one APIService, so only one adapter
can serve external metrics per cluster. Installing a second one either fails
(`APIService "v1beta1.external.metrics.k8s.io" ... exists and cannot be imported
into the current release`) or silently replaces the first, after which the HPAs
of the first adapter stop scaling.

- [kedacore/keda#470](https://github.com/kedacore/keda/issues/470) "KEDA might
  break existing deployment on cluster which already has another External
  Metrics Adapter installed": 93 reactions, 65 comments, open since 11/2019.
  Reports with Datadog, prometheus-adapter, kube-state-metrics, the CloudWatch
  adapter, the Stackdriver adapter and Google Managed Prometheus. Maintainers:
  "The limitation is at k8s level, we can't provide any solution from KEDA side"
  (2022), "We are still blocked by Kubernetes upstream" (2024).
- [kubernetes-sigs/custom-metrics-apiserver#70](https://github.com/kubernetes-sigs/custom-metrics-apiserver/issues/70)
  "Support multiple custom/external metrics servers in the cluster": 99
  reactions, open since 10/2020. A SIG Instrumentation maintainer on migrating
  between adapters: "Sadly not, there is no easy migration process today because
  you can't have two different source of external metrics."
- [KEP-4261](https://github.com/kubernetes/enhancements/pull/4262) (multiple
  metrics APIs) was closed in 10/2025 without being implemented.
- Earlier proofs of concept exist and are unmaintained:
  [barkbay/metrics-router](https://github.com/barkbay/metrics-router) (2021),
  [arjunrn/custom-metrics-router](https://github.com/arjunrn/custom-metrics-router) (2021),
  [Pluies/external-metrics-multiplexer](https://github.com/Pluies/external-metrics-multiplexer)
  (nginx, routes by renamed metric prefixes, last commit 01/2023). Routing only by
  metric name or prefix does not work for KEDA: KEDA writes the HPAs itself and
  its metric names (`s0-redis-jobs`) are not listed in discovery.

## What it does

- Serves `external.metrics.k8s.io/v1beta1` with the standard aggregated API
  server machinery (sigs.k8s.io/custom-metrics-apiserver): delegated
  authentication and authorization, so a caller without RBAC on external
  metrics gets `Forbidden` from metrics-mux before anything is forwarded.
- Routes each request, in this order:
  1. **routes**: the first matching rule by metric name regex, label selector
     key (KEDA always selects on `scaledobject.keda.sh/name`) and namespace,
     optionally stripping a prefix;
  2. **discovery**: the first backend (in config order) whose
     `GET /apis/external.metrics.k8s.io/v1beta1` lists the metric;
  3. **fallback** backend, if configured; otherwise `NotFound`.
- Calls backends over HTTPS with its ServiceAccount token. Backend CAs can be
  inline, read from a Secret (re-read every 30 s, so KEDA's rotated
  `kedaorg-certs` CA is picked up) or not verified.
- Lists the union of all backends' metrics in discovery. A backend whose
  discovery fails keeps its last list; `/status` on port 8080 shows the state.
- Issues its own serving certificate into a Secret and keeps the APIService
  pointed at itself with the matching `caBundle`. If an adapter or a Helm
  upgrade re-registers the APIService, metrics-mux takes it back at the next
  refresh (`--takeover=false` only reports it).
- `metrics-mux explain` lists every External metric of every HPA with the
  backend it routes to; `--query` also fetches the current value through the
  kube-apiserver.

## Install

There is no published image; build it and push it to your registry:

```sh
docker build -t <registry>/metrics-mux:0.1.0 .
docker push <registry>/metrics-mux:0.1.0
```

1. Install KEDA without its APIService, so it does not compete for it:

   ```sh
   helm upgrade --install keda kedacore/keda -n keda --create-namespace \
     --set metricsServer.registerAPIService=false
   ```

   Other adapters can stay as they are; metrics-mux rewrites the existing
   APIService when it starts.
2. Edit the ConfigMap in [deploy/metrics-mux.yaml](deploy/metrics-mux.yaml),
   set the image, and apply it. The manifest includes RBAC for reading KEDA's
   CA Secret; adjust it for other CA Secrets.
3. Check the routing:

   ```sh
   kubectl get apiservice v1beta1.external.metrics.k8s.io
   go run ./cmd/metrics-mux explain --config config.yaml --query
   ```

The configuration is read at startup; after changing the ConfigMap run
`kubectl -n metrics-mux rollout restart deploy/metrics-mux`.
`metrics-mux check-config --config file` validates a file offline.

## Configuration

```yaml
backends:                      # priority order for discovery
- name: keda
  service: {namespace: keda, name: keda-operator-metrics-apiserver, port: 443}
  caSecret: {namespace: keda, name: kedaorg-certs, key: ca.crt}
- name: prometheus
  service: {namespace: monitoring, name: prometheus-adapter, port: 443}
  insecureSkipTLSVerify: true  # prometheus-adapter's chart default is a self-signed cert
- name: cloudwatch
  url: https://cloudwatch-adapter.custom-metrics.svc:443
  caBundle: |
    -----BEGIN CERTIFICATE-----
    ...
  timeout: 5s                  # default 10s
routes:
- backend: keda
  selectorKeys: [scaledobject.keda.sh/name, scaledjob.keda.sh/name]
- backend: cloudwatch
  metric: "cw-.*"              # whole-name regex
  stripPrefix: cw-             # the backend receives the name without it
  namespaces: [shop]           # optional
discovery: true                # default
fallback: prometheus           # optional
```

## Example

`explain` against the end-to-end test cluster (KEDA ScaledObject plus an HPA on a
prometheus-adapter metric):

```
EXPLAIN_OUTPUT
```

Unit test output for the routing table, from `internal/explain`:

```
NAMESPACE  HPA              METRIC         SELECTOR                          BACKEND     REASON
jobs       keda-hpa-worker  s0-redis-jobs  scaledobject.keda.sh/name=worker  keda        route 0 (selectorKeys)
jobs       legacy           queue_depth    -                                 ?           discovery (decided at request time)
shop       web              prom-http_rps  -                                 prometheus  route 1 (metric) as http_rps
```

## Tests

- `go test -race ./...`: config validation, routing order, anchored regexes,
  backend client against TLS test servers (token, selector, Status
  pass-through, CA reload after rotation), discovery union and stale
  discovery, certificate issue/reuse/renewal, APIService create/takeover.
- [e2e/run.sh](e2e/run.sh) (in CI on every push and weekly): kind cluster,
  Prometheus, prometheus-adapter (which registers the APIService first), KEDA
  with `registerAPIService=false`, metrics-mux. It checks that metrics-mux takes
  over the APIService, that it becomes Available, that both an HPA on a
  prometheus-adapter metric and a KEDA ScaledObject's HPA are `ScalingActive`
  with current values, that an unprivileged ServiceAccount gets `Forbidden`,
  that `explain --query` reports both routes, and that a re-registration by
  the adapter is undone.

## Limitations

- Only `external.metrics.k8s.io`. `custom.metrics.k8s.io` (Object and Pods
  metrics) is not multiplexed yet.
- Backends see metrics-mux's ServiceAccount, not the original caller.
  metrics-mux authorizes the caller itself; the backend only authorizes
  metrics-mux.
- Tested with KEDA and prometheus-adapter only. The Datadog Cluster Agent is
  not tested; it builds its queries from all HPAs in the cluster, which may
  include metrics that belong to other adapters.
- When two backends list the same metric name, discovery picks the first in
  config order; add a route to make it explicit.
- The serving certificate is self-signed (CA 10 years, leaf 1 year, renewed
  in-process 30 days before expiry). There is no cert-manager integration.
- Configuration changes need a restart.

## License

MIT
