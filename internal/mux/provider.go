// Package mux implements the external metrics provider that forwards requests
// to the backend chosen by the router.
package mux

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"k8s.io/metrics/pkg/apis/external_metrics"
	"k8s.io/metrics/pkg/apis/external_metrics/v1beta1"
	"sigs.k8s.io/custom-metrics-apiserver/pkg/provider"

	"github.com/Jwrede/metrics-mux/internal/backend"
	"github.com/Jwrede/metrics-mux/internal/router"
)

// BackendStatus is the last discovery result of one backend.
type BackendStatus struct {
	Metrics   int
	Err       error
	CheckedAt time.Time
}

// Provider forwards external metric requests. It implements provider.ExternalMetricsProvider.
type Provider struct {
	router   *router.Router
	backends []*backend.Client
	byName   map[string]*backend.Client

	mu       sync.RWMutex
	listed   map[string][]string // metric -> backends listing it, in backend order
	all      []provider.ExternalMetricInfo
	statuses map[string]BackendStatus
}

// New creates a provider. backends must be in priority order.
func New(r *router.Router, backends []*backend.Client) *Provider {
	p := &Provider{router: r, backends: backends, byName: map[string]*backend.Client{},
		listed: map[string][]string{}, statuses: map[string]BackendStatus{}}
	for _, b := range backends {
		p.byName[b.Name] = b
	}
	return p
}

// Refresh reloads CA bundles and the metric lists of all backends. A backend
// whose discovery fails keeps its previously listed metrics.
func (p *Provider) Refresh(ctx context.Context) {
	type result struct {
		names []string
		err   error
	}
	results := make([]result, len(p.backends))
	var wg sync.WaitGroup
	for i, b := range p.backends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Refresh(ctx); err != nil {
				results[i] = result{err: err}
				return
			}
			names, err := b.ListMetrics(ctx)
			results[i] = result{names: names, err: err}
		}()
	}
	wg.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.listed
	listed := map[string][]string{}
	now := time.Now()
	for i, b := range p.backends {
		res := results[i]
		names := res.names
		if res.err != nil {
			klog.Warningf("discovery of backend %s failed: %v", b.Name, res.err)
			names = nil
			for m, bs := range old {
				if slices.Contains(bs, b.Name) {
					names = append(names, m)
				}
			}
			st := p.statuses[b.Name]
			st.Err, st.CheckedAt = res.err, now
			p.statuses[b.Name] = st
		} else {
			p.statuses[b.Name] = BackendStatus{Metrics: len(names), CheckedAt: now}
		}
		for _, m := range names {
			if !slices.Contains(listed[m], b.Name) {
				listed[m] = append(listed[m], b.Name)
			}
		}
	}
	all := make([]provider.ExternalMetricInfo, 0, len(listed))
	for m := range listed {
		all = append(all, provider.ExternalMetricInfo{Metric: m})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Metric < all[j].Metric })
	p.listed, p.all = listed, all
}

// Run refreshes discovery until ctx is done.
func (p *Provider) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		p.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Lookup returns the backends that listed a metric at the last refresh.
func (p *Provider) Lookup(metric string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.listed[metric])
}

// Statuses returns a copy of the per-backend discovery status.
func (p *Provider) Statuses() map[string]BackendStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]BackendStatus, len(p.statuses))
	for k, v := range p.statuses {
		out[k] = v
	}
	return out
}

// Route resolves the backend for a request using the current discovery data.
func (p *Provider) Route(namespace, metric string, selector labels.Selector) (router.Decision, bool) {
	return p.router.Resolve(namespace, metric, selector, p.Lookup)
}

// GetExternalMetric implements provider.ExternalMetricsProvider.
func (p *Provider) GetExternalMetric(ctx context.Context, namespace string, selector labels.Selector, info provider.ExternalMetricInfo) (*external_metrics.ExternalMetricValueList, error) {
	d, ok := p.Route(namespace, info.Metric, selector)
	if !ok {
		klog.V(2).Infof("no backend for external metric %s/%s selector=%q", namespace, info.Metric, selector)
		return nil, provider.NewMetricNotFoundError(v1beta1.Resource(info.Metric), info.Metric)
	}
	b := p.byName[d.Backend]
	start := time.Now()
	list, err := b.GetMetric(ctx, namespace, d.Metric, selector)
	klog.V(2).Infof("external metric %s/%s selector=%q -> %s as %q [%s] in %s err=%v",
		namespace, info.Metric, selector, d.Backend, d.Metric, d.Reason, time.Since(start).Round(time.Millisecond), err)
	if err != nil {
		return nil, err
	}
	out := &external_metrics.ExternalMetricValueList{}
	if err := v1beta1.Convert_v1beta1_ExternalMetricValueList_To_external_metrics_ExternalMetricValueList(list, out, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// ListAllExternalMetrics implements provider.ExternalMetricsProvider: the union
// of all backends' listed metrics.
func (p *Provider) ListAllExternalMetrics() []provider.ExternalMetricInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.all)
}
