// Package takeover keeps the metrics-mux serving certificate in a Secret and
// points the external metrics APIService at metrics-mux.
package takeover

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Secret keys.
const (
	KeyCA    = "ca.crt"
	KeyCAKey = "ca.key"
	KeyCert  = "tls.crt"
	KeyKey   = "tls.key"
)

// RenewBefore is how long before expiry a serving certificate is replaced.
const RenewBefore = 30 * 24 * time.Hour

// Bundle is a CA plus a serving certificate signed by it.
type Bundle struct {
	CA, CAKey, Cert, Key []byte
}

// Now is replaceable in tests.
var Now = time.Now

// EnsureCerts returns a valid bundle for dnsNames from the Secret, creating or
// replacing it when it is missing, unparsable, lacks a name or expires within
// RenewBefore. A CA that stays valid for two more years is kept, so the
// APIService caBundle does not change on routine renewals. The bool result is
// true when a new certificate was issued.
func EnsureCerts(ctx context.Context, kube kubernetes.Interface, namespace, name string, dnsNames []string) (*Bundle, bool, error) {
	secrets := kube.CoreV1().Secrets(namespace)
	for attempt := 0; attempt < 3; attempt++ {
		s, err := secrets.Get(ctx, name, metav1.GetOptions{})
		exists := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, false, err
		}
		var cur *Bundle
		if exists {
			cur = &Bundle{CA: s.Data[KeyCA], CAKey: s.Data[KeyCAKey], Cert: s.Data[KeyCert], Key: s.Data[KeyKey]}
			if ValidFor(cur, dnsNames, Now()) == nil {
				return cur, false, nil
			}
		}
		next, err := issue(cur, dnsNames)
		if err != nil {
			return nil, false, err
		}
		data := map[string][]byte{KeyCA: next.CA, KeyCAKey: next.CAKey, KeyCert: next.Cert, KeyKey: next.Key}
		if exists {
			s.Data = data
			_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
		} else {
			_, err = secrets.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
					Labels: map[string]string{"app.kubernetes.io/managed-by": "metrics-mux"}},
				Type: corev1.SecretTypeOpaque, Data: data}, metav1.CreateOptions{})
		}
		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			continue // another replica wrote first; re-read its result
		}
		if err != nil {
			return nil, false, err
		}
		return next, true, nil
	}
	return nil, false, fmt.Errorf("secret %s/%s: too many conflicts", namespace, name)
}

// ValidFor checks that the bundle parses, the certificate chains to the CA,
// covers all dnsNames and is still valid RenewBefore after now.
func ValidFor(b *Bundle, dnsNames []string, now time.Time) error {
	if _, err := tls.X509KeyPair(b.Cert, b.Key); err != nil {
		return fmt.Errorf("key pair: %w", err)
	}
	leaf, err := parseCert(b.Cert)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CA) {
		return fmt.Errorf("no CA certificate")
	}
	for _, n := range dnsNames {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: n, Roots: pool, CurrentTime: now.Add(RenewBefore)}); err != nil {
			return fmt.Errorf("verify %s: %w", n, err)
		}
	}
	return nil
}

func parseCert(p []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(p)
	if blk == nil {
		return nil, fmt.Errorf("no PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func encodeKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// issue signs a new serving certificate (valid one year), reusing the CA in cur
// when possible and otherwise creating a new CA (valid ten years).
func issue(cur *Bundle, dnsNames []string) (*Bundle, error) {
	now := Now()
	caCert, caKey, caPEM, caKeyPEM := reusableCA(cur, now)
	if caCert == nil {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		tmpl := &x509.Certificate{
			SerialNumber:          serial(),
			Subject:               pkix.Name{CommonName: "metrics-mux-ca"},
			NotBefore:             now.Add(-time.Hour),
			NotAfter:              now.AddDate(10, 0, 0),
			KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			return nil, err
		}
		if caCert, err = x509.ParseCertificate(der); err != nil {
			return nil, err
		}
		caKey = key
		caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		if caKeyPEM, err = encodeKey(key); err != nil {
			return nil, err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	return &Bundle{
		CA:    caPEM,
		CAKey: caKeyPEM,
		Cert:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:   keyPEM,
	}, nil
}

func reusableCA(cur *Bundle, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte) {
	if cur == nil {
		return nil, nil, nil, nil
	}
	ca, err := parseCert(cur.CA)
	if err != nil || !ca.IsCA || now.AddDate(2, 0, 0).After(ca.NotAfter) {
		return nil, nil, nil, nil
	}
	blk, _ := pem.Decode(cur.CAKey)
	if blk == nil {
		return nil, nil, nil, nil
	}
	k, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil || !k.PublicKey.Equal(ca.PublicKey) {
		return nil, nil, nil, nil
	}
	return ca, k, bytes.Clone(cur.CA), bytes.Clone(cur.CAKey)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}
