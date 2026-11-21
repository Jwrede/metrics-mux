// Command metrics-mux serves external.metrics.k8s.io and forwards each request
// to one of several external metrics adapters.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
	basecmd "sigs.k8s.io/custom-metrics-apiserver/pkg/cmd"

	"github.com/Jwrede/metrics-mux/internal/backend"
	"github.com/Jwrede/metrics-mux/internal/config"
	"github.com/Jwrede/metrics-mux/internal/explain"
	"github.com/Jwrede/metrics-mux/internal/mux"
	"github.com/Jwrede/metrics-mux/internal/router"
	"github.com/Jwrede/metrics-mux/internal/takeover"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "explain":
			os.Exit(runExplain(os.Args[2:]))
		case "check-config":
			os.Exit(runCheck(os.Args[2:]))
		case "version":
			fmt.Println(version)
			return
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}
	if err := runServe(); err != nil {
		klog.Fatal(err)
	}
}

func runCheck(args []string) int {
	fs := pflag.NewFlagSet("check-config", pflag.ExitOnError)
	path := fs.String("config", "/etc/metrics-mux/config.yaml", "configuration file")
	_ = fs.Parse(args)
	c, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("ok: %d backends, %d routes, discovery=%v, fallback=%q\n", len(c.Backends), len(c.Routes), c.DiscoveryEnabled(), c.Fallback)
	return 0
}

func runExplain(args []string) int {
	fs := pflag.NewFlagSet("explain", pflag.ExitOnError)
	path := fs.String("config", "config.yaml", "metrics-mux configuration file")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	namespace := fs.StringP("namespace", "n", "", "only HPAs in this namespace (default: all)")
	query := fs.Bool("query", false, "also request each metric through the kube-apiserver and show the value")
	_ = fs.Parse(args)
	c, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if *kubeconfig != "" {
		rules.ExplicitPath = *kubeconfig
	}
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	hpas, err := kube.AutoscalingV2().HorizontalPodAutoscalers(*namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rows := explain.Rows(hpas.Items, router.New(c))
	if *query {
		explain.Query(ctx, kube, rows)
	}
	if err := explain.Print(os.Stdout, rows, *query); err != nil {
		return 1
	}
	return 0
}

type options struct {
	configPath       string
	tokenFile        string
	refresh          time.Duration
	certSecret       string
	serviceNamespace string
	serviceName      string
	servicePort      int32
	manageAPIService bool
	takeover         bool
	healthAddr       string
}

func runServe() error {
	logs.InitLogs()
	defer logs.FlushLogs()

	cmd := &basecmd.AdapterBase{Name: "metrics-mux"}
	var o options
	fs := cmd.Flags()
	fs.StringVar(&o.configPath, "config", "/etc/metrics-mux/config.yaml", "configuration file")
	fs.StringVar(&o.tokenFile, "backend-token-file", "/var/run/secrets/kubernetes.io/serviceaccount/token", "bearer token sent to backends (empty: none)")
	fs.DurationVar(&o.refresh, "refresh-interval", 30*time.Second, "interval for backend discovery, CA reload and APIService reconciliation")
	fs.StringVar(&o.certSecret, "cert-secret", "", "namespace/name of the Secret holding the generated serving certificate (empty: use --tls-cert-file)")
	fs.StringVar(&o.serviceNamespace, "service-namespace", os.Getenv("POD_NAMESPACE"), "namespace of the metrics-mux Service")
	fs.StringVar(&o.serviceName, "service-name", "metrics-mux", "name of the metrics-mux Service")
	fs.Int32Var(&o.servicePort, "service-port", 443, "port of the metrics-mux Service")
	fs.BoolVar(&o.manageAPIService, "manage-apiservice", true, "create or update APIService v1beta1.external.metrics.k8s.io to point at metrics-mux (needs --cert-secret)")
	fs.BoolVar(&o.takeover, "takeover", true, "with --manage-apiservice: rewrite the APIService even when it points at another adapter")
	fs.StringVar(&o.healthAddr, "status-address", ":8080", "plain HTTP address for /healthz and /status (empty: off)")
	logs.AddFlags(fs)
	if err := fs.Parse(os.Args); err != nil {
		return err
	}

	c, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(o.tokenFile); o.tokenFile != "" && err != nil {
		klog.Warningf("backend token file %s: %v; sending no token", o.tokenFile, err)
		o.tokenFile = ""
	}

	restCfg, err := cmd.ClientConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var clients []*backend.Client
	for _, b := range c.Backends {
		var src backend.CASource
		if ref := b.CASecret; ref != nil {
			key := ref.Key
			if key == "" {
				key = "ca.crt"
			}
			src = func(ctx context.Context) ([]byte, error) {
				s, err := kube.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				if len(s.Data[key]) == 0 {
					return nil, fmt.Errorf("secret %s/%s has no key %q", ref.Namespace, ref.Name, key)
				}
				return s.Data[key], nil
			}
		}
		clients = append(clients, backend.New(b, o.tokenFile, src))
	}
	prov := mux.New(router.New(c), clients)
	prov.Refresh(ctx)
	go prov.Run(ctx, o.refresh)

	if o.certSecret != "" {
		if err := setupCerts(ctx, cmd, kube, dyn, o); err != nil {
			return err
		}
	} else if o.manageAPIService {
		klog.Info("--cert-secret not set: not managing the APIService")
	}

	if o.healthAddr != "" {
		go serveStatus(o.healthAddr, prov)
	}

	cmd.WithExternalMetrics(prov)
	klog.Infof("metrics-mux %s: %d backends, %d routes", version, len(c.Backends), len(c.Routes))
	return cmd.Run(ctx)
}

func setupCerts(ctx context.Context, cmd *basecmd.AdapterBase, kube kubernetes.Interface, dyn dynamic.Interface, o options) error {
	ns, name, ok := splitRef(o.certSecret)
	if !ok {
		return fmt.Errorf("--cert-secret must be namespace/name")
	}
	if o.serviceNamespace == "" {
		return fmt.Errorf("--service-namespace (or POD_NAMESPACE) is required with --cert-secret")
	}
	dnsNames := []string{
		o.serviceName + "." + o.serviceNamespace + ".svc",
		o.serviceName + "." + o.serviceNamespace + ".svc.cluster.local",
	}
	dir, err := os.MkdirTemp("", "metrics-mux-certs")
	if err != nil {
		return err
	}
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	var caBundle []byte
	sync := func() error {
		b, issued, err := takeover.EnsureCerts(ctx, kube, ns, name, dnsNames)
		if err != nil {
			return err
		}
		if issued {
			klog.Infof("issued serving certificate for %v into secret %s", dnsNames, o.certSecret)
		}
		if err := writeIfChanged(certFile, b.Cert); err != nil {
			return err
		}
		if err := writeIfChanged(keyFile, b.Key); err != nil {
			return err
		}
		caBundle = b.CA
		if !o.manageAPIService {
			return nil
		}
		res, err := takeover.Reconcile(ctx, dyn, takeover.Target{
			Group: "external.metrics.k8s.io", Version: "v1beta1",
			ServiceNamespace: o.serviceNamespace, ServiceName: o.serviceName, ServicePort: o.servicePort,
			CABundle: caBundle,
		}, o.takeover)
		if err != nil {
			return err
		}
		switch res.Action {
		case "created":
			klog.Info("created APIService v1beta1.external.metrics.k8s.io")
		case "updated":
			klog.Infof("updated APIService v1beta1.external.metrics.k8s.io (was %s)", res.Previous)
		}
		return nil
	}
	if err := sync(); err != nil {
		return err
	}
	cmd.InstallFlags()
	cmd.SecureServing.ServerCert.CertKey.CertFile = certFile
	cmd.SecureServing.ServerCert.CertKey.KeyFile = keyFile
	go func() {
		t := time.NewTicker(o.refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := sync(); err != nil {
					klog.Errorf("certificate/APIService reconcile: %v", err)
				}
			}
		}
	}()
	return nil
}

func writeIfChanged(path string, data []byte) error {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == string(data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func splitRef(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], i > 0 && i < len(s)-1
		}
	}
	return "", "", false
}

func serveStatus(addr string, p *mux.Provider) {
	m := http.NewServeMux()
	m.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	m.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		sts := p.Statuses()
		names := make([]string, 0, len(sts))
		for n := range sts {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			st := sts[name]
			errText := "-"
			if st.Err != nil {
				errText = st.Err.Error()
			}
			fmt.Fprintf(w, "backend=%s metrics=%d checked=%s error=%s\n", name, st.Metrics, st.CheckedAt.UTC().Format(time.RFC3339), errText)
		}
	})
	srv := &http.Server{Addr: addr, Handler: m, ReadHeaderTimeout: 5 * time.Second}
	klog.Error(srv.ListenAndServe())
}
