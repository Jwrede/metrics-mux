package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/Jwrede/metrics-mux/internal/config"
	"github.com/Jwrede/metrics-mux/internal/testadapter"
)

func TestGetAndList(t *testing.T) {
	a := testadapter.New(map[string]string{"queue": "42"}, "queue", "other")
	defer a.Close()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("s3cr3t"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := New(config.Backend{Name: "a", URL: a.URL, CABundle: string(a.CAPEM()), ServerName: "example.com"}, tok, nil)
	ctx := context.Background()

	names, err := c.ListMetrics(ctx)
	if err != nil || len(names) != 2 || names[0] != "queue" {
		t.Fatalf("list: %v %v", names, err)
	}
	sel := labels.SelectorFromSet(labels.Set{"app": "web", "scaledobject.keda.sh/name": "w"})
	list, err := c.GetMetric(ctx, "shop", "queue", sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Value.String() != "42" {
		t.Fatalf("items %+v", list.Items)
	}
	got := a.Got()[0]
	if got.Namespace != "shop" || got.Selector != sel.String() || got.Auth != "Bearer s3cr3t" {
		t.Errorf("request %+v", got)
	}
}

func TestBackendStatusIsPassedThrough(t *testing.T) {
	a := testadapter.New(map[string]string{}, "x")
	defer a.Close()
	c := New(config.Backend{Name: "a", URL: a.URL, InsecureSkipTLSVerify: true}, "", nil)
	_, err := c.GetMetric(context.Background(), "ns", "missing", nil)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
	a.SetFailList(true)
	_, err = c.ListMetrics(context.Background())
	var se *apierrors.StatusError
	if !errors.As(err, &se) || se.ErrStatus.Code != 503 {
		t.Fatalf("want 503 status error, got %v", err)
	}
}

func TestUnverifiedCertificateFails(t *testing.T) {
	a := testadapter.New(map[string]string{"m": "1"}, "m")
	defer a.Close()
	c := New(config.Backend{Name: "a", URL: a.URL}, "", nil)
	if _, err := c.ListMetrics(context.Background()); !apierrors.IsServiceUnavailable(err) {
		t.Fatalf("want ServiceUnavailable for unknown CA, got %v", err)
	}
}

func TestCASourceRotation(t *testing.T) {
	cur := testadapter.New(map[string]string{"m": "1"}, "m")
	defer cur.Close()
	ca := testadapter.OtherCAPEM() // wrong CA first, as if the backend rotated its certificate
	src := func(context.Context) ([]byte, error) { return ca, nil }
	c := New(config.Backend{Name: "a", URL: cur.URL, ServerName: "example.com"}, "", src)
	ctx := context.Background()
	if _, err := c.ListMetrics(ctx); err == nil {
		t.Fatal("request before Refresh must fail (CA not loaded)")
	}
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListMetrics(ctx); err == nil {
		t.Fatal("old CA must not verify the new certificate")
	}
	ca = cur.CAPEM()
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListMetrics(ctx); err != nil {
		t.Fatalf("after CA reload: %v", err)
	}
}
