package router

import (
	"testing"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/Jwrede/metrics-mux/internal/config"
)

func mustRouter(t *testing.T, y string) *Router {
	t.Helper()
	c, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return New(c)
}

const cfg = `
backends:
- {name: keda, url: 'https://k'}
- {name: prom, url: 'https://p'}
- {name: dd, url: 'https://d'}
routes:
- backend: keda
  selectorKeys: [scaledobject.keda.sh/name, scaledjob.keda.sh/name]
- backend: prom
  metric: "prom-.*"
  stripPrefix: prom-
- backend: dd
  metric: "datadogmetric@.*|dd\\..*"
  namespaces: [shop]
fallback: prom
`

func TestResolve(t *testing.T) {
	r := mustRouter(t, cfg)
	kedaSel := labels.SelectorFromSet(labels.Set{"scaledobject.keda.sh/name": "worker"})
	lookup := func(m string) []string {
		if m == "queue_depth" {
			return []string{"dd", "prom"}
		}
		return nil
	}
	cases := []struct {
		ns, metric string
		sel        labels.Selector
		backend    string
		sent       string
		reason     string
	}{
		{"default", "s0-prometheus", kedaSel, "keda", "s0-prometheus", "route 0 (selectorKeys)"},
		{"default", "prom-http_rps", labels.Everything(), "prom", "http_rps", "route 1 (metric)"},
		{"default", "xprom-http_rps", labels.Everything(), "prom", "xprom-http_rps", "fallback"},
		{"shop", "dd.queue", labels.Everything(), "dd", "dd.queue", "route 2 (metric,namespaces)"},
		{"other", "dd.queue", labels.Everything(), "prom", "dd.queue", "fallback"},
		{"other", "queue_depth", labels.Everything(), "dd", "queue_depth", "discovery (also listed by prom)"},
		// a route beats discovery
		{"default", "prom-queue", kedaSel, "keda", "prom-queue", "route 0 (selectorKeys)"},
	}
	for _, c := range cases {
		d, ok := r.Resolve(c.ns, c.metric, c.sel, lookup)
		if !ok || d.Backend != c.backend || d.Metric != c.sent || d.Reason != c.reason {
			t.Errorf("%s/%s: got %+v ok=%v, want %s %s %q", c.ns, c.metric, d, ok, c.backend, c.sent, c.reason)
		}
	}
}

func TestMetricRegexIsAnchored(t *testing.T) {
	r := mustRouter(t, `
backends:
- {name: a, url: 'https://a'}
routes:
- {backend: a, metric: "a|ab"}
discovery: false
`)
	if _, ok := r.Resolve("ns", "ab", nil, nil); !ok {
		t.Error("ab should match a|ab as a whole")
	}
	if _, ok := r.Resolve("ns", "abc", nil, nil); ok {
		t.Error("abc must not match a|ab")
	}
}

func TestNoMatchWithoutFallback(t *testing.T) {
	r := mustRouter(t, `
backends:
- {name: a, url: 'https://a'}
discovery: false
`)
	if d, ok := r.Resolve("ns", "m", nil, func(string) []string { return []string{"a"} }); ok {
		t.Errorf("discovery disabled, want no match, got %+v", d)
	}
}
