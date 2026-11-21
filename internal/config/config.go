// Package config defines the metrics-mux configuration file.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the content of the YAML configuration file.
type Config struct {
	// Backends are the external metrics adapters behind the mux, in priority order.
	Backends []Backend `json:"backends"`
	// Routes are evaluated in order; the first matching route wins.
	Routes []Route `json:"routes,omitempty"`
	// Discovery enables routing by the metric names each backend lists
	// (GET /apis/external.metrics.k8s.io/v1beta1). Defaults to true.
	Discovery *bool `json:"discovery,omitempty"`
	// Fallback is the backend used when no route and no discovered name matches.
	Fallback string `json:"fallback,omitempty"`
}

// Backend is one external metrics adapter.
type Backend struct {
	Name string `json:"name"`
	// Service addresses the adapter as https://<name>.<namespace>.svc:<port>.
	Service *ServiceRef `json:"service,omitempty"`
	// URL addresses the adapter directly (alternative to Service).
	URL string `json:"url,omitempty"`
	// CABundle is a PEM CA bundle that signed the adapter's serving certificate.
	CABundle string `json:"caBundle,omitempty"`
	// CASecret reads the CA bundle from a Secret key; it is re-read on every
	// discovery cycle, so rotated certificates (KEDA's kedaorg-certs) are picked up.
	CASecret *SecretKeyRef `json:"caSecret,omitempty"`
	// InsecureSkipTLSVerify disables verification of the adapter's certificate.
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
	// ServerName overrides the TLS server name (default <name>.<namespace>.svc).
	ServerName string `json:"serverName,omitempty"`
	// Timeout per request, e.g. "10s". Default 10s.
	Timeout string `json:"timeout,omitempty"`
}

// ServiceRef points at a Kubernetes Service.
type ServiceRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Port      int32  `json:"port,omitempty"`
}

// SecretKeyRef points at one key of a Secret.
type SecretKeyRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Key       string `json:"key,omitempty"`
}

// Route sends matching metric requests to a backend. All set fields must match.
type Route struct {
	Backend string `json:"backend"`
	// Metric is a regular expression matched against the full metric name.
	Metric string `json:"metric,omitempty"`
	// SelectorKeys matches when the request's label selector has a requirement
	// on any of these keys (KEDA always sets scaledobject.keda.sh/name).
	SelectorKeys []string `json:"selectorKeys,omitempty"`
	// Namespaces restricts the route to requests from these namespaces.
	Namespaces []string `json:"namespaces,omitempty"`
	// StripPrefix is removed from the metric name before it is sent to the backend.
	StripPrefix string `json:"stripPrefix,omitempty"`
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses and validates configuration YAML.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// DiscoveryEnabled reports whether discovery-based routing is on.
func (c *Config) DiscoveryEnabled() bool { return c.Discovery == nil || *c.Discovery }

// Validate checks references and field combinations.
func (c *Config) Validate() error {
	if len(c.Backends) == 0 {
		return fmt.Errorf("config: at least one backend is required")
	}
	names := map[string]bool{}
	for i, b := range c.Backends {
		if b.Name == "" {
			return fmt.Errorf("config: backends[%d]: name is required", i)
		}
		if names[b.Name] {
			return fmt.Errorf("config: duplicate backend name %q", b.Name)
		}
		names[b.Name] = true
		if (b.Service == nil) == (b.URL == "") {
			return fmt.Errorf("config: backend %q: set exactly one of service and url", b.Name)
		}
		if b.Service != nil && (b.Service.Name == "" || b.Service.Namespace == "") {
			return fmt.Errorf("config: backend %q: service needs namespace and name", b.Name)
		}
		n := 0
		if b.CABundle != "" {
			n++
		}
		if b.CASecret != nil {
			n++
			if b.CASecret.Name == "" || b.CASecret.Namespace == "" {
				return fmt.Errorf("config: backend %q: caSecret needs namespace and name", b.Name)
			}
		}
		if b.InsecureSkipTLSVerify {
			n++
		}
		if n > 1 {
			return fmt.Errorf("config: backend %q: caBundle, caSecret and insecureSkipTLSVerify are mutually exclusive", b.Name)
		}
		if b.Timeout != "" {
			if _, err := time.ParseDuration(b.Timeout); err != nil {
				return fmt.Errorf("config: backend %q: timeout: %w", b.Name, err)
			}
		}
	}
	for i, r := range c.Routes {
		if !names[r.Backend] {
			return fmt.Errorf("config: routes[%d]: unknown backend %q", i, r.Backend)
		}
		if r.Metric == "" && len(r.SelectorKeys) == 0 && len(r.Namespaces) == 0 {
			return fmt.Errorf("config: routes[%d]: set at least one of metric, selectorKeys, namespaces", i)
		}
		if r.Metric != "" {
			if _, err := regexp.Compile("^(?:" + r.Metric + ")$"); err != nil {
				return fmt.Errorf("config: routes[%d]: metric: %w", i, err)
			}
		}
	}
	if c.Fallback != "" && !names[c.Fallback] {
		return fmt.Errorf("config: fallback: unknown backend %q", c.Fallback)
	}
	return nil
}

// TimeoutOrDefault returns the parsed timeout or 10s.
func (b Backend) TimeoutOrDefault() time.Duration {
	if d, err := time.ParseDuration(b.Timeout); err == nil && d > 0 {
		return d
	}
	return 10 * time.Second
}

// Address returns the base URL of the backend.
func (b Backend) Address() string {
	if b.URL != "" {
		return b.URL
	}
	port := b.Service.Port
	if port == 0 {
		port = 443
	}
	return fmt.Sprintf("https://%s.%s.svc:%d", b.Service.Name, b.Service.Namespace, port)
}

// TLSServerName returns the server name to verify.
func (b Backend) TLSServerName() string {
	if b.ServerName != "" {
		return b.ServerName
	}
	if b.Service != nil {
		return b.Service.Name + "." + b.Service.Namespace + ".svc"
	}
	return ""
}
