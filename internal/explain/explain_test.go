package explain

import (
	"bytes"
	"strings"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Jwrede/metrics-mux/internal/config"
	"github.com/Jwrede/metrics-mux/internal/router"
)

func hpa(ns, name, metric string, sel map[string]string) autoscalingv2.HorizontalPodAutoscaler {
	var ls *metav1.LabelSelector
	if sel != nil {
		ls = &metav1.LabelSelector{MatchLabels: sel}
	}
	return autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{Metrics: []autoscalingv2.MetricSpec{
			{Type: autoscalingv2.ResourceMetricSourceType},
			{Type: autoscalingv2.ExternalMetricSourceType, External: &autoscalingv2.ExternalMetricSource{
				Metric: autoscalingv2.MetricIdentifier{Name: metric, Selector: ls}}},
		}},
	}
}

func TestRowsAndPrint(t *testing.T) {
	c, err := config.Parse([]byte(`
backends:
- {name: keda, url: 'https://k'}
- {name: prometheus, url: 'https://p'}
routes:
- {backend: keda, selectorKeys: [scaledobject.keda.sh/name]}
- {backend: prometheus, metric: 'prom-.*', stripPrefix: prom-}
`))
	if err != nil {
		t.Fatal(err)
	}
	rows := Rows([]autoscalingv2.HorizontalPodAutoscaler{
		hpa("shop", "web", "prom-http_rps", nil),
		hpa("jobs", "keda-hpa-worker", "s0-redis-jobs", map[string]string{"scaledobject.keda.sh/name": "worker"}),
		hpa("jobs", "legacy", "queue_depth", nil),
	}, router.New(c))
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	var buf bytes.Buffer
	if err := Print(&buf, rows, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Log("\n" + out)
	for _, want := range []string{
		"jobs       keda-hpa-worker  s0-redis-jobs  scaledobject.keda.sh/name=worker  keda        route 0 (selectorKeys)",
		"jobs       legacy           queue_depth    -                                 ?           discovery (decided at request time)",
		"shop       web              prom-http_rps  -                                 prometheus  route 1 (metric) as http_rps",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing line %q", want)
		}
	}
}
