package config

import (
	"strings"
	"testing"
)

const good = `
backends:
- name: keda
  service: {namespace: keda, name: keda-operator-metrics-apiserver}
  caSecret: {namespace: keda, name: kedaorg-certs, key: ca.crt}
- name: prometheus
  url: https://127.0.0.1:6443
  insecureSkipTLSVerify: true
  timeout: 3s
routes:
- backend: keda
  selectorKeys: [scaledobject.keda.sh/name]
- backend: prometheus
  metric: "prom_.*"
  stripPrefix: prom_
fallback: prometheus
`

func TestParseGood(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if !c.DiscoveryEnabled() {
		t.Error("discovery should default to true")
	}
	k := c.Backends[0]
	if got := k.Address(); got != "https://keda-operator-metrics-apiserver.keda.svc:443" {
		t.Errorf("address %s", got)
	}
	if got := k.TLSServerName(); got != "keda-operator-metrics-apiserver.keda.svc" {
		t.Errorf("server name %s", got)
	}
	if got := k.TimeoutOrDefault().String(); got != "10s" {
		t.Errorf("default timeout %s", got)
	}
	if got := c.Backends[1].TimeoutOrDefault().String(); got != "3s" {
		t.Errorf("timeout %s", got)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no backends":     `backends: []`,
		"unknown field":   "backends:\n- name: a\n  url: https://x\n  bogus: 1",
		"duplicate":       "backends:\n- {name: a, url: 'https://x'}\n- {name: a, url: 'https://y'}",
		"both addr":       "backends:\n- name: a\n  url: https://x\n  service: {namespace: n, name: s}",
		"no addr":         "backends:\n- name: a",
		"ca exclusive":    "backends:\n- name: a\n  url: https://x\n  caBundle: x\n  insecureSkipTLSVerify: true",
		"unknown backend": "backends:\n- {name: a, url: 'https://x'}\nroutes:\n- {backend: b, metric: x}",
		"empty route":     "backends:\n- {name: a, url: 'https://x'}\nroutes:\n- {backend: a}",
		"bad regex":       "backends:\n- {name: a, url: 'https://x'}\nroutes:\n- {backend: a, metric: '('}",
		"bad fallback":    "backends:\n- {name: a, url: 'https://x'}\nfallback: b",
		"bad timeout":     "backends:\n- {name: a, url: 'https://x', timeout: soon}",
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if !strings.Contains(err.Error(), "config") {
			t.Errorf("%s: unexpected error text %v", name, err)
		}
	}
}
