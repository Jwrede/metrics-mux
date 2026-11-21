// Package explain shows, for every External metric used by an HPA, which
// backend metrics-mux routes it to.
package explain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/metrics/pkg/apis/external_metrics/v1beta1"

	"github.com/Jwrede/metrics-mux/internal/router"
)

// Row is one External metric reference of one HPA.
type Row struct {
	Namespace, HPA, Metric, Selector string
	Backend, Reason                  string
	// Value is filled when querying: the summed value or an error message.
	Value string
}

// Rows builds the routing table for the given HPAs. Discovery-based routes
// cannot be evaluated offline; such metrics show "discovery" as reason with the
// fallback backend (if any).
func Rows(hpas []autoscalingv2.HorizontalPodAutoscaler, r *router.Router) []Row {
	var rows []Row
	for _, h := range hpas {
		for _, m := range h.Spec.Metrics {
			if m.Type != autoscalingv2.ExternalMetricSourceType || m.External == nil {
				continue
			}
			sel := labels.Everything()
			if m.External.Metric.Selector != nil {
				s, err := metav1.LabelSelectorAsSelector(m.External.Metric.Selector)
				if err != nil {
					rows = append(rows, Row{Namespace: h.Namespace, HPA: h.Name, Metric: m.External.Metric.Name, Reason: "invalid selector: " + err.Error()})
					continue
				}
				sel = s
			}
			row := Row{Namespace: h.Namespace, HPA: h.Name, Metric: m.External.Metric.Name, Selector: sel.String()}
			if d, ok := r.Resolve(h.Namespace, row.Metric, sel, nil); ok {
				row.Backend, row.Reason = d.Backend, d.Reason
				if d.Metric != row.Metric {
					row.Reason += " as " + d.Metric
				}
			} else if r.DiscoveryEnabled() {
				row.Backend, row.Reason = "?", "discovery (decided at request time)"
			} else {
				row.Backend, row.Reason = "-", "no route: request fails with NotFound"
			}
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].HPA < rows[j].HPA
	})
	return rows
}

// Query fills Value by requesting each metric through the kube-apiserver,
// i.e. through whatever currently serves external.metrics.k8s.io.
func Query(ctx context.Context, kube kubernetes.Interface, rows []Row) {
	for i := range rows {
		r := &rows[i]
		req := kube.Discovery().RESTClient().Get().
			AbsPath("/apis/external.metrics.k8s.io/v1beta1/namespaces", r.Namespace, r.Metric)
		if r.Selector != "" {
			req = req.Param("labelSelector", r.Selector)
		}
		body, err := req.DoRaw(ctx)
		if err != nil {
			r.Value = "error: " + firstLine(err.Error())
			continue
		}
		var list v1beta1.ExternalMetricValueList
		if err := json.Unmarshal(body, &list); err != nil {
			r.Value = "error: " + err.Error()
			continue
		}
		vals := make([]string, 0, len(list.Items))
		for _, it := range list.Items {
			vals = append(vals, it.Value.String())
		}
		r.Value = strings.Join(vals, ",")
		if r.Value == "" {
			r.Value = "(no items)"
		}
	}
}

// Print writes the rows as a table.
func Print(w io.Writer, rows []Row, withValue bool) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	h := "NAMESPACE\tHPA\tMETRIC\tSELECTOR\tBACKEND\tREASON"
	if withValue {
		h += "\tVALUE"
	}
	fmt.Fprintln(tw, h)
	for _, r := range rows {
		sel := r.Selector
		if sel == "" {
			sel = "-"
		}
		line := strings.Join([]string{r.Namespace, r.HPA, r.Metric, sel, r.Backend, r.Reason}, "\t")
		if withValue {
			line += "\t" + r.Value
		}
		fmt.Fprintln(tw, line)
	}
	return tw.Flush()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
