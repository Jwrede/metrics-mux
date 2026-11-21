#!/usr/bin/env bash
# End-to-end test: kind cluster with KEDA and prometheus-adapter behind metrics-mux.
# Needs docker, kind, kubectl, helm. KEEP_CLUSTER=1 keeps the cluster afterwards.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-metrics-mux-e2e}
KEDA_CHART_VERSION=${KEDA_CHART_VERSION:-}
ADAPTER_CHART_VERSION=${ADAPTER_CHART_VERSION:-}
PROM_IMAGE=${PROM_IMAGE:-quay.io/prometheus/prometheus:v3.5.0}

log() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; dump; exit 1; }
dump() {
  set +e
  kubectl get apiservice v1beta1.external.metrics.k8s.io -o yaml
  kubectl -n metrics-mux logs deploy/metrics-mux --tail=80
  kubectl -n e2e describe hpa
  kubectl get pods -A
}
cleanup() { [ "${KEEP_CLUSTER:-}" = 1 ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

# wait_for <seconds> <description> <command...>: retry until the command succeeds.
wait_for() {
  local t=$1 what=$2; shift 2
  local end=$((SECONDS + t))
  until "$@" >/dev/null 2>&1; do
    [ $SECONDS -lt $end ] || fail "timeout after ${t}s: $what"
    sleep 3
  done
  echo "ok: $what"
}

log "cluster"
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
docker build -q -t metrics-mux:e2e .
kind load docker-image metrics-mux:e2e --name "$CLUSTER"

log "prometheus (scrapes itself) and prometheus-adapter with one external rule"
kubectl create namespace monitoring --dry-run=client -o yaml | kubectl apply -f -
kubectl -n monitoring create deployment prometheus --image="$PROM_IMAGE" --port=9090 --dry-run=client -o yaml | kubectl apply -f -
kubectl -n monitoring expose deployment prometheus --port=9090 --dry-run=client -o yaml | kubectl apply -f -
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
helm repo add kedacore https://kedacore.github.io/charts >/dev/null
helm repo update >/dev/null
helm upgrade --install prometheus-adapter prometheus-community/prometheus-adapter -n monitoring \
  ${ADAPTER_CHART_VERSION:+--version "$ADAPTER_CHART_VERSION"} -f e2e/prometheus-adapter-values.yaml --wait --timeout 5m
wait_for 180 "prometheus-adapter serves prom_up (APIService still owned by the adapter)" \
  kubectl get --raw /apis/external.metrics.k8s.io/v1beta1/namespaces/default/prom_up

log "KEDA with its APIService registration disabled (the one supported way to coexist)"
helm upgrade --install keda kedacore/keda -n keda --create-namespace \
  ${KEDA_CHART_VERSION:+--version "$KEDA_CHART_VERSION"} --set metricsServer.registerAPIService=false --wait --timeout 5m
helm list -A
wait_for 120 "KEDA CA secret exists" kubectl -n keda get secret kedaorg-certs

log "metrics-mux"
sed 's#image: metrics-mux:latest#image: metrics-mux:e2e#' deploy/metrics-mux.yaml | kubectl apply -f -
kubectl -n metrics-mux rollout status deploy/metrics-mux --timeout=180s
apiservice_is_mux() {
  [ "$(kubectl get apiservice v1beta1.external.metrics.k8s.io -o jsonpath='{.spec.service.namespace}/{.spec.service.name}')" = metrics-mux/metrics-mux ]
}
apiservice_available() {
  [ "$(kubectl get apiservice v1beta1.external.metrics.k8s.io -o jsonpath='{.status.conditions[?(@.type=="Available")].status}')" = True ]
}
wait_for 60 "APIService taken over from prometheus-adapter" apiservice_is_mux
wait_for 120 "APIService Available" apiservice_available
kubectl -n metrics-mux logs deploy/metrics-mux | grep -E 'updated APIService|issued serving' || fail "no takeover log line"

log "workloads: one HPA on a prometheus-adapter metric, one KEDA ScaledObject"
kubectl apply -f e2e/workloads.yaml
wait_for 120 "KEDA created its HPA" kubectl -n e2e get hpa keda-hpa-worker

hpa_active() {
  [ "$(kubectl -n e2e get hpa "$1" -o jsonpath='{.status.conditions[?(@.type=="ScalingActive")].status}')" = True ] &&
  [ -n "$(kubectl -n e2e get hpa "$1" -o jsonpath='{.status.currentMetrics[0].external.current}')" ]
}
wait_for 240 "HPA web (prometheus-adapter metric) is ScalingActive with a current value" hpa_active web
wait_for 240 "HPA keda-hpa-worker (KEDA metric) is ScalingActive with a current value" hpa_active keda-hpa-worker

log "results"
kubectl -n e2e get hpa
kubectl -n e2e get hpa web -o jsonpath='web current: {.status.currentMetrics[0].external.current}{"\n"}'
kubectl -n e2e get hpa keda-hpa-worker -o jsonpath='keda-hpa-worker current: {.status.currentMetrics[0].external.current}{"\n"}'
disc=$(kubectl get --raw /apis/external.metrics.k8s.io/v1beta1)
echo "$disc" | grep -q '"prom_up"' || fail "discovery union lacks prom_up: $disc"
raw=$(kubectl get --raw /apis/external.metrics.k8s.io/v1beta1/namespaces/e2e/prom_up)
echo "$raw"
echo "$raw" | grep -q '"value":"1"' || fail "prom_up through metrics-mux: $raw"

log "explain --query (run from outside the cluster)"
cat > /tmp/mux-config.yaml <<'CFG'
backends:
- name: keda
  service: {namespace: keda, name: keda-operator-metrics-apiserver, port: 443}
  caSecret: {namespace: keda, name: kedaorg-certs, key: ca.crt}
- name: prometheus
  service: {namespace: monitoring, name: prometheus-adapter, port: 443}
  insecureSkipTLSVerify: true
routes:
- backend: keda
  selectorKeys: [scaledobject.keda.sh/name, scaledjob.keda.sh/name]
CFG
go run ./cmd/metrics-mux explain --config /tmp/mux-config.yaml -n e2e --query | tee /tmp/explain.txt
grep -E 'keda-hpa-worker .* keda +route 0 \(selectorKeys\) +[0-9]' /tmp/explain.txt || fail "explain: KEDA row"
grep -E '^e2e +web +prom_up .* \? +discovery .* 1$' /tmp/explain.txt || fail "explain: prometheus row"

log "authorization is enforced by metrics-mux"
kubectl -n e2e create serviceaccount nobody --dry-run=client -o yaml | kubectl apply -f -
if out=$(kubectl get --raw /apis/external.metrics.k8s.io/v1beta1/namespaces/e2e/prom_up --as=system:serviceaccount:e2e:nobody 2>&1); then
  fail "unprivileged ServiceAccount could read metrics: $out"
fi
echo "$out" | grep -qi forbidden || fail "expected Forbidden, got: $out"
echo "ok: $out"

log "an adapter re-registering the APIService is undone"
kubectl patch apiservice v1beta1.external.metrics.k8s.io --type=merge \
  -p '{"spec":{"service":{"namespace":"monitoring","name":"prometheus-adapter","port":443},"caBundle":null,"insecureSkipTLSVerify":true}}'
wait_for 90 "APIService points at metrics-mux again" apiservice_is_mux
wait_for 120 "APIService Available again" apiservice_available

log "routing log"
kubectl -n metrics-mux logs deploy/metrics-mux --all-containers --prefix=false | grep 'external metric' | tail -4
echo
echo "E2E PASSED"
