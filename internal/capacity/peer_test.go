package capacity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// pki makes a CA, a server certificate for host and a client certificate, and
// writes the CA and the client's pair as PEM files in dir.
func pki(t *testing.T, dir, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	write := func(name, kind string, der []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	caKey := key()
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	write("ca.crt", "CERTIFICATE", caDER)
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	issue := func(serial int64, usage x509.ExtKeyUsage, dns []string) ([]byte, *ecdsa.PrivateKey) {
		k := key()
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "peer"}, NotBefore: time.Now().Add(-time.Hour),
			NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: dns}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return der, k
	}
	serverDER, serverKey := issue(2, x509.ExtKeyUsageServerAuth, []string{host})
	clientDER, clientKey := issue(3, x509.ExtKeyUsageClientAuth, nil)
	write("tls.crt", "CERTIFICATE", clientDER)
	keyDER, _ := x509.MarshalECPrivateKey(clientKey)
	write("tls.key", "EC PRIVATE KEY", keyDER)
	return tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}, pool
}

// A prefill/decode deployment is measured through the gateway's peer
// listener, which asks for the site's client certificate. AIPerf cannot
// present one, so the runner passes its requests on: here it reads the
// model's length through that path and stops as the model is too short.
func TestRunnerPeer(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	dir := t.TempDir()
	cert, pool := pki(t, dir, "peers.llm.test")
	var sni, host string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		fmt.Fprint(w, `{"object":"list","data":[{"id":"m","object":"model","max_model_len":8192}]}`)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) { sni = h.ServerName; return nil, nil }}
	server.StartTLS()
	defer server.Close()

	set := Defaults(0)
	config, _ := json.Marshal(map[string]any{"url": "http://unused", "model": "m", "settings": set, "grace_seconds": 1, "agentic": Agentic(set),
		"metrics_urls": []string{"http://127.0.0.1:1"},
		"peer": map[string]string{"address": server.Listener.Addr().String(), "sni": "peers.llm.test",
			"cert": filepath.Join(dir, "tls.crt"), "key": filepath.Join(dir, "tls.key"), "ca": filepath.Join(dir, "ca.crt")}})
	cmd := exec.Command(python, "-c", runner)
	cmd.Env = append(os.Environ(), "AIGW_CAPACITY="+string(config), "AIGW_WORK="+dir)
	out, _ := cmd.CombinedOutput()
	l := ParseLog(string(out))
	if l.Error == "" || l.Error[:29] != "the model takes at most 8,192" || sni != "peers.llm.test" || host != "peers.llm.test" {
		t.Fatalf("error %q, sni %q, host %q\n%s", l.Error, sni, host, out)
	}
}
