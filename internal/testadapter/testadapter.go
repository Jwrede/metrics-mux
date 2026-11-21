// Package testadapter is a fake external metrics adapter for tests.
package testadapter

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/metrics/pkg/apis/external_metrics/v1beta1"
)

// Request is one metric request the adapter received.
type Request struct {
	Namespace, Metric, Selector, Auth string
}

// Adapter serves discovery and fixed metric values over TLS.
type Adapter struct {
	*httptest.Server
	mu       sync.Mutex
	Metrics  map[string]string // metric -> value
	Listed   []string          // names returned by discovery
	FailList bool
	Requests []Request
}

// New starts an adapter. Values are served for metrics in values; listed is
// what discovery returns.
func New(values map[string]string, listed ...string) *Adapter {
	a := &Adapter{Metrics: values, Listed: listed}
	a.Server = httptest.NewTLSServer(http.HandlerFunc(a.serve))
	return a
}

// CAPEM returns the PEM of the server certificate (self-signed).
func (a *Adapter) CAPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.Certificate().Raw})
}

// SetFailList toggles discovery failures.
func (a *Adapter) SetFailList(v bool) { a.mu.Lock(); a.FailList = v; a.mu.Unlock() }

// Got returns the received metric requests.
func (a *Adapter) Got() []Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Request(nil), a.Requests...)
}

func (a *Adapter) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	const root = "/apis/external.metrics.k8s.io/v1beta1"
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == root {
		if a.FailList {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("down"))
			return
		}
		list := metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: "external.metrics.k8s.io/v1beta1"}
		for _, n := range a.Listed {
			list.APIResources = append(list.APIResources, metav1.APIResource{Name: n, Namespaced: true, Kind: "ExternalMetricValueList", Verbs: []string{"get"}})
		}
		_ = json.NewEncoder(w).Encode(list)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, root+"/"), "/")
	if len(parts) != 3 || parts[0] != "namespaces" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	req := Request{Namespace: parts[1], Metric: parts[2], Selector: r.URL.Query().Get("labelSelector"), Auth: r.Header.Get("Authorization")}
	a.Requests = append(a.Requests, req)
	v, ok := a.Metrics[req.Metric]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status: "Failure", Code: 404, Reason: metav1.StatusReasonNotFound, Message: "metric " + req.Metric + " not found"})
		return
	}
	list := v1beta1.ExternalMetricValueList{TypeMeta: metav1.TypeMeta{Kind: "ExternalMetricValueList", APIVersion: "external.metrics.k8s.io/v1beta1"},
		Items: []v1beta1.ExternalMetricValue{{MetricName: req.Metric, MetricLabels: map[string]string{"src": "test"}, Timestamp: metav1.Now(), Value: resource.MustParse(v)}}}
	_ = json.NewEncoder(w).Encode(list)
}

// OtherCAPEM returns a freshly generated self-signed CA that did not sign the adapter certificate.
func OtherCAPEM() []byte {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
