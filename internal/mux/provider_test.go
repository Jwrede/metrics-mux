package mux

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/provider"

	"github.com/Jwrede/metrics-mux/internal/backend"
	"github.com/Jwrede/metrics-mux/internal/config"
	"github.com/Jwrede/metrics-mux/internal/router"
	"github.com/Jwrede/metrics-mux/internal/testadapter"
)

func setup(t *testing.T) (*Provider, *testadapter.Adapter, *testadapter.Adapter) {
	t.Helper()
	// KEDA lists nothing useful in discovery; its HPAs carry scaledobject.keda.sh/name.
	keda := testadapter.New(map[string]string{"s0-redis-jobs": "7", "http_rps": "999"})
	prom := testadapter.New(map[string]string{"http_rps": "12", "queue_depth": "3"}, "http_rps", "queue_depth")
	t.Cleanup(keda.Close)
	t.Cleanup(prom.Close)
	c, err := config.Parse([]byte(`
backends:
- {name: keda, url: '` + keda.URL + `', insecureSkipTLSVerify: true}
- {name: prometheus, url: '` + prom.URL + `', insecureSkipTLSVerify: true}
routes:
- backend: keda
  selectorKeys: [scaledobject.keda.sh/name]
`))
	if err != nil {
		t.Fatal(err)
	}
	var clients []*backend.Client
	for _, b := range c.Backends {
		clients = append(clients, backend.New(b, "", nil))
	}
	p := New(router.New(c), clients)
	p.Refresh(context.Background())
	return p, keda, prom
}

func value(t *testing.T, p *Provider, ns, metric string, sel labels.Selector) string {
	t.Helper()
	list, err := p.GetExternalMetric(context.Background(), ns, sel, provider.ExternalMetricInfo{Metric: metric})
	if err != nil {
		t.Fatalf("%s: %v", metric, err)
	}
	return list.Items[0].Value.String()
}

func TestRoutesBySelectorThenDiscovery(t *testing.T) {
	p, keda, prom := setup(t)
	kedaSel := labels.SelectorFromSet(labels.Set{"scaledobject.keda.sh/name": "worker"})
	if v := value(t, p, "default", "s0-redis-jobs", kedaSel); v != "7" {
		t.Errorf("keda metric = %s", v)
	}
	if v := value(t, p, "default", "http_rps", labels.Everything()); v != "12" {
		t.Errorf("prometheus metric = %s (want value from prometheus, not keda)", v)
	}
	if len(keda.Got()) != 1 || len(prom.Got()) != 1 {
		t.Errorf("requests keda=%v prom=%v", keda.Got(), prom.Got())
	}
	_, err := p.GetExternalMetric(context.Background(), "default", labels.Everything(), provider.ExternalMetricInfo{Metric: "unknown"})
	if !apierrors.IsNotFound(err) {
		t.Errorf("unrouted metric: want NotFound, got %v", err)
	}
}

func TestListUnionAndStaleDiscovery(t *testing.T) {
	p, _, prom := setup(t)
	all := p.ListAllExternalMetrics()
	if len(all) != 2 || all[0].Metric != "http_rps" || all[1].Metric != "queue_depth" {
		t.Fatalf("union %+v", all)
	}
	prom.SetFailList(true)
	p.Refresh(context.Background())
	if got := p.Lookup("queue_depth"); len(got) != 1 || got[0] != "prometheus" {
		t.Errorf("failed discovery must keep the previous list, got %v", got)
	}
	if st := p.Statuses()["prometheus"]; st.Err == nil || st.Metrics != 2 {
		t.Errorf("status %+v", st)
	}
	if v := value(t, p, "default", "queue_depth", labels.Everything()); v != "3" {
		t.Errorf("value %s", v)
	}
}
