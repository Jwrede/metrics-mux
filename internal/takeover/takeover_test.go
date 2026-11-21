package takeover

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

var names = []string{"metrics-mux.metrics-mux.svc", "metrics-mux.metrics-mux.svc.cluster.local"}

func TestEnsureCertsCreateReuseRenew(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewSimpleClientset()
	b1, issued, err := EnsureCerts(ctx, kube, "metrics-mux", "serving", names)
	if err != nil || !issued {
		t.Fatalf("first: issued=%v err=%v", issued, err)
	}
	if err := ValidFor(b1, names, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := ValidFor(b1, []string{"other.svc"}, time.Now()); err == nil {
		t.Error("certificate must not be valid for other names")
	}
	b2, issued, err := EnsureCerts(ctx, kube, "metrics-mux", "serving", names)
	if err != nil || issued || !bytes.Equal(b1.Cert, b2.Cert) {
		t.Fatalf("second call must reuse the secret: issued=%v err=%v", issued, err)
	}

	// 340 days later the leaf is inside the renewal window; the CA is kept.
	defer func() { Now = time.Now }()
	Now = func() time.Time { return time.Now().Add(340 * 24 * time.Hour) }
	b3, issued, err := EnsureCerts(ctx, kube, "metrics-mux", "serving", names)
	if err != nil || !issued {
		t.Fatalf("renewal: issued=%v err=%v", issued, err)
	}
	if bytes.Equal(b3.Cert, b1.Cert) || !bytes.Equal(b3.CA, b1.CA) {
		t.Error("renewal must issue a new leaf from the same CA")
	}
	s, _ := kube.CoreV1().Secrets("metrics-mux").Get(ctx, "serving", metav1.GetOptions{})
	if !bytes.Equal(s.Data[KeyCert], b3.Cert) {
		t.Error("secret not updated")
	}

	// A new service name forces a new leaf.
	Now = time.Now
	b4, issued, err := EnsureCerts(ctx, kube, "metrics-mux", "serving", append(names, "extra.svc"))
	if err != nil || !issued || ValidFor(b4, []string{"extra.svc"}, time.Now()) != nil {
		t.Fatalf("new name: issued=%v err=%v", issued, err)
	}
}

func apiservice(svcNS, svcName string, ca []byte, skip bool) *unstructured.Unstructured {
	spec := map[string]any{
		"group": "external.metrics.k8s.io", "version": "v1beta1",
		"groupPriorityMinimum": int64(100), "versionPriority": int64(100),
		"service": map[string]any{"namespace": svcNS, "name": svcName, "port": int64(443)},
	}
	if ca != nil {
		spec["caBundle"] = base64.StdEncoding.EncodeToString(ca)
	}
	if skip {
		spec["insecureSkipTLSVerify"] = true
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiregistration.k8s.io/v1", "kind": "APIService",
		"metadata": map[string]any{"name": "v1beta1.external.metrics.k8s.io"},
		"spec":     spec,
	}}
}

func newDyn(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{APIServiceGVR: "APIServiceList"}, objs...)
}

var target = Target{Group: "external.metrics.k8s.io", Version: "v1beta1",
	ServiceNamespace: "metrics-mux", ServiceName: "metrics-mux", ServicePort: 443, CABundle: []byte("CA")}

func spec(t *testing.T, dyn *dynfake.FakeDynamicClient) map[string]any {
	t.Helper()
	u, err := dyn.Resource(APIServiceGVR).Get(context.Background(), "v1beta1.external.metrics.k8s.io", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := unstructured.NestedMap(u.Object, "spec")
	return s
}

func TestReconcileCreates(t *testing.T) {
	dyn := newDyn()
	res, err := Reconcile(context.Background(), dyn, target, true)
	if err != nil || res.Action != "created" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := serviceString(spec(t, dyn)); got != "metrics-mux/metrics-mux:443" {
		t.Errorf("service %s", got)
	}
	res, err = Reconcile(context.Background(), dyn, target, true)
	if err != nil || res.Action != "none" {
		t.Fatalf("second reconcile must be a no-op: %+v %v", res, err)
	}
}

func TestReconcileTakesOver(t *testing.T) {
	dyn := newDyn(apiservice("keda", "keda-operator-metrics-apiserver", nil, true))
	if _, err := Reconcile(context.Background(), dyn, target, false); err == nil || !strings.Contains(err.Error(), "keda/keda-operator-metrics-apiserver:443") {
		t.Fatalf("takeover disabled: want error naming the current owner, got %v", err)
	}
	res, err := Reconcile(context.Background(), dyn, target, true)
	if err != nil || res.Action != "updated" || res.Previous != "keda/keda-operator-metrics-apiserver:443" {
		t.Fatalf("%+v %v", res, err)
	}
	s := spec(t, dyn)
	if _, ok := s["insecureSkipTLSVerify"]; ok {
		t.Error("insecureSkipTLSVerify must be removed")
	}
	if s["caBundle"] != base64.StdEncoding.EncodeToString([]byte("CA")) {
		t.Errorf("caBundle %v", s["caBundle"])
	}
}

func TestReconcileFixesCABundleWithoutTakeover(t *testing.T) {
	// Own service but stale CA: allowed even with takeover disabled.
	dyn := newDyn(apiservice("metrics-mux", "metrics-mux", []byte("OLD"), false))
	res, err := Reconcile(context.Background(), dyn, target, false)
	if err != nil || res.Action != "updated" {
		t.Fatalf("%+v %v", res, err)
	}
}
