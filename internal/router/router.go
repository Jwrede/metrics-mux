// Package router decides which backend serves an external metric request.
package router

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/Jwrede/metrics-mux/internal/config"
)

// Decision is the result of routing one request.
type Decision struct {
	Backend string
	// Metric is the name sent to the backend (after StripPrefix).
	Metric string
	// Reason explains the decision, e.g. "route 0 (selectorKeys)".
	Reason string
}

type route struct {
	config.Route
	re *regexp.Regexp
}

// Router evaluates routes, then discovery, then the fallback.
type Router struct {
	routes    []route
	discovery bool
	fallback  string
}

// New builds a router from a validated config.
func New(c *config.Config) *Router {
	r := &Router{discovery: c.DiscoveryEnabled(), fallback: c.Fallback}
	for _, cr := range c.Routes {
		rt := route{Route: cr}
		if cr.Metric != "" {
			rt.re = regexp.MustCompile("^(?:" + cr.Metric + ")$")
		}
		r.routes = append(r.routes, rt)
	}
	return r
}

// Lookup returns the backends (in priority order) that list a metric name.
type Lookup func(metric string) []string

// Resolve picks a backend. lookup may be nil when discovery data is unavailable.
func (r *Router) Resolve(namespace, metric string, selector labels.Selector, lookup Lookup) (Decision, bool) {
	keys := selectorKeys(selector)
	for i, rt := range r.routes {
		var why []string
		if rt.re != nil {
			if !rt.re.MatchString(metric) {
				continue
			}
			why = append(why, "metric")
		}
		if len(rt.SelectorKeys) > 0 {
			if !slices.ContainsFunc(rt.SelectorKeys, func(k string) bool { return keys[k] }) {
				continue
			}
			why = append(why, "selectorKeys")
		}
		if len(rt.Namespaces) > 0 {
			if !slices.Contains(rt.Namespaces, namespace) {
				continue
			}
			why = append(why, "namespaces")
		}
		return Decision{
			Backend: rt.Backend,
			Metric:  strings.TrimPrefix(metric, rt.StripPrefix),
			Reason:  fmt.Sprintf("route %d (%s)", i, strings.Join(why, ",")),
		}, true
	}
	if r.discovery && lookup != nil {
		if bs := lookup(metric); len(bs) > 0 {
			reason := "discovery"
			if len(bs) > 1 {
				reason = fmt.Sprintf("discovery (also listed by %s)", strings.Join(bs[1:], ","))
			}
			return Decision{Backend: bs[0], Metric: metric, Reason: reason}, true
		}
	}
	if r.fallback != "" {
		return Decision{Backend: r.fallback, Metric: metric, Reason: "fallback"}, true
	}
	return Decision{}, false
}

// DiscoveryEnabled reports whether Resolve consults the lookup.
func (r *Router) DiscoveryEnabled() bool { return r.discovery }

func selectorKeys(sel labels.Selector) map[string]bool {
	keys := map[string]bool{}
	if sel == nil {
		return keys
	}
	reqs, _ := sel.Requirements()
	for _, req := range reqs {
		keys[req.Key()] = true
	}
	return keys
}
