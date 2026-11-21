package takeover

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// APIServiceGVR is apiregistration.k8s.io/v1 apiservices.
var APIServiceGVR = schema.GroupVersionResource{Group: "apiregistration.k8s.io", Version: "v1", Resource: "apiservices"}

// Target describes the APIService metrics-mux should own.
type Target struct {
	Group, Version   string
	ServiceNamespace string
	ServiceName      string
	ServicePort      int32
	CABundle         []byte
}

// Name is the APIService object name, e.g. v1beta1.external.metrics.k8s.io.
func (t Target) Name() string { return t.Version + "." + t.Group }

// Result reports what Reconcile did.
type Result struct {
	Action string // "none", "created", "updated"
	// Previous is the service the APIService pointed at before an update ("" if none or local).
	Previous string
}

// Reconcile creates the APIService or rewrites its service reference and
// caBundle so that the kube-apiserver sends the group to metrics-mux. When
// takeover is false an APIService pointing elsewhere is left alone and an error is returned.
func Reconcile(ctx context.Context, dyn dynamic.Interface, t Target, takeover bool) (Result, error) {
	api := dyn.Resource(APIServiceGVR)
	obj, err := api.Get(ctx, t.Name(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apiregistration.k8s.io/v1",
			"kind":       "APIService",
			"metadata": map[string]any{
				"name":   t.Name(),
				"labels": map[string]any{"app.kubernetes.io/managed-by": "metrics-mux"},
			},
			"spec": desiredSpec(t, nil),
		}}
		if _, err := api.Create(ctx, u, metav1.CreateOptions{}); err != nil {
			return Result{}, fmt.Errorf("create APIService %s: %w", t.Name(), err)
		}
		return Result{Action: "created"}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("get APIService %s: %w", t.Name(), err)
	}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if matches(spec, t) {
		return Result{Action: "none"}, nil
	}
	prev := serviceString(spec)
	if !takeover && prev != serviceString(desiredSpec(t, nil)) {
		return Result{Previous: prev}, fmt.Errorf("APIService %s points at %s; not taking over (takeover disabled)", t.Name(), prev)
	}
	if err := unstructured.SetNestedMap(obj.Object, desiredSpec(t, spec), "spec"); err != nil {
		return Result{}, err
	}
	if _, err := api.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return Result{}, fmt.Errorf("update APIService %s: %w", t.Name(), err)
	}
	return Result{Action: "updated", Previous: prev}, nil
}

func desiredSpec(t Target, cur map[string]any) map[string]any {
	spec := map[string]any{}
	for k, v := range cur {
		spec[k] = v
	}
	spec["group"] = t.Group
	spec["version"] = t.Version
	spec["service"] = map[string]any{"namespace": t.ServiceNamespace, "name": t.ServiceName, "port": int64(t.ServicePort)}
	spec["caBundle"] = base64.StdEncoding.EncodeToString(t.CABundle)
	delete(spec, "insecureSkipTLSVerify")
	if _, ok := spec["groupPriorityMinimum"]; !ok {
		spec["groupPriorityMinimum"] = int64(100)
	}
	if _, ok := spec["versionPriority"]; !ok {
		spec["versionPriority"] = int64(100)
	}
	return spec
}

func matches(spec map[string]any, t Target) bool {
	if spec == nil || serviceString(spec) != serviceString(desiredSpec(t, nil)) {
		return false
	}
	if skip, _, _ := unstructured.NestedBool(spec, "insecureSkipTLSVerify"); skip {
		return false
	}
	ca, _, _ := unstructured.NestedString(spec, "caBundle")
	raw, err := base64.StdEncoding.DecodeString(ca)
	return err == nil && bytes.Equal(raw, t.CABundle)
}

func serviceString(spec map[string]any) string {
	svc, ok, _ := unstructured.NestedMap(spec, "service")
	if !ok || svc == nil {
		return ""
	}
	port := int64(443)
	switch p := svc["port"].(type) {
	case int64:
		port = p
	case float64:
		port = int64(p)
	case int32:
		port = int64(p)
	case int:
		port = int64(p)
	}
	return fmt.Sprintf("%v/%v:%d", svc["namespace"], svc["name"], port)
}
