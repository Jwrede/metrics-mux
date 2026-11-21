// Package backend talks to one external metrics adapter over HTTPS.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"k8s.io/metrics/pkg/apis/external_metrics/v1beta1"

	"github.com/Jwrede/metrics-mux/internal/config"
)

// APIPath is the external metrics API root served by every adapter.
const APIPath = "/apis/external.metrics.k8s.io/v1beta1"

// CASource returns the current CA bundle (PEM). It is called before requests
// are built and on every Refresh, so rotated CAs are picked up.
type CASource func(ctx context.Context) ([]byte, error)

// Client is a client for one backend adapter. It is safe for concurrent use.
type Client struct {
	Name      string
	cfg       config.Backend
	tokenFile string
	caSource  CASource

	mu     sync.Mutex
	ca     []byte
	client *http.Client
}

// New creates a client. tokenFile (may be empty) holds the bearer token sent to
// the backend; caSource may be nil when the CA comes from the config or is not verified.
func New(cfg config.Backend, tokenFile string, caSource CASource) *Client {
	c := &Client{Name: cfg.Name, cfg: cfg, tokenFile: tokenFile, caSource: caSource}
	if cfg.CABundle != "" {
		c.ca = []byte(cfg.CABundle)
	}
	return c
}

// Refresh re-reads the CA bundle from its source and rebuilds the transport if it changed.
func (c *Client) Refresh(ctx context.Context) error {
	if c.caSource == nil {
		_, err := c.httpClient()
		return err
	}
	ca, err := c.caSource(ctx)
	if err != nil {
		return fmt.Errorf("backend %s: read CA: %w", c.Name, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !bytes.Equal(ca, c.ca) || c.client == nil {
		c.ca = ca
		c.client = nil
	}
	return nil
}

func (c *Client) httpClient() (*http.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	if c.caSource != nil && c.ca == nil {
		return nil, fmt.Errorf("backend %s: CA bundle not loaded yet", c.Name)
	}
	rc := &rest.Config{
		Host:            c.cfg.Address(),
		BearerTokenFile: c.tokenFile,
		Timeout:         c.cfg.TimeoutOrDefault(),
		TLSClientConfig: rest.TLSClientConfig{
			CAData:     c.ca,
			Insecure:   c.cfg.InsecureSkipTLSVerify,
			ServerName: c.cfg.TLSServerName(),
		},
	}
	hc, err := rest.HTTPClientFor(rc)
	if err != nil {
		return nil, fmt.Errorf("backend %s: %w", c.Name, err)
	}
	c.client = hc
	return hc, nil
}

// ListMetrics returns the metric names the backend advertises in discovery.
func (c *Client) ListMetrics(ctx context.Context) ([]string, error) {
	var list metav1.APIResourceList
	if err := c.get(ctx, APIPath, &list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.APIResources))
	for _, r := range list.APIResources {
		names = append(names, r.Name)
	}
	return names, nil
}

// GetMetric fetches one external metric for a namespace and selector.
func (c *Client) GetMetric(ctx context.Context, namespace, metric string, selector labels.Selector) (*v1beta1.ExternalMetricValueList, error) {
	p := fmt.Sprintf("%s/namespaces/%s/%s", APIPath, url.PathEscape(namespace), url.PathEscape(metric))
	if selector != nil && !selector.Empty() {
		p += "?labelSelector=" + url.QueryEscape(selector.String())
	}
	var list v1beta1.ExternalMetricValueList
	if err := c.get(ctx, p, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	hc, err := c.httpClient()
	if err != nil {
		return apierrors.NewServiceUnavailable(err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Address()+path, nil)
	if err != nil {
		return apierrors.NewInternalError(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return apierrors.NewServiceUnavailable(fmt.Sprintf("backend %s: %v", c.Name, err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return apierrors.NewServiceUnavailable(fmt.Sprintf("backend %s: read body: %v", c.Name, err))
	}
	if resp.StatusCode != http.StatusOK {
		var st metav1.Status
		if json.Unmarshal(body, &st) == nil && st.Kind == "Status" {
			if st.Code == 0 {
				st.Code = int32(resp.StatusCode)
			}
			st.Message = fmt.Sprintf("backend %s: %s", c.Name, st.Message)
			return &apierrors.StatusError{ErrStatus: st}
		}
		return apierrors.NewGenericServerResponse(resp.StatusCode, http.MethodGet,
			v1beta1.Resource("externalmetrics"), "", fmt.Sprintf("backend %s: %s", c.Name, truncate(body)), 0, false)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return apierrors.NewInternalError(fmt.Errorf("backend %s: decode response: %w", c.Name, err))
	}
	return nil
}

func truncate(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(bytes.TrimSpace(b))
}
