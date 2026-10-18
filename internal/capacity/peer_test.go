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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
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
		// Too short for any measurement: the Job stops right after it asked.
		fmt.Fprint(w, `{"object":"list","data":[{"id":"m","object":"model","max_model_len":1200}]}`)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) { sni = h.ServerName; return nil, nil }}
	server.StartTLS()
	defer server.Close()

	set := Defaults(0)
	config, _ := json.Marshal(map[string]any{"model": "m", "settings": set, "grace_seconds": 1, "agentic": Agentic(set),
		"metrics_urls": []string{"http://127.0.0.1:1"},
		"peer": map[string]string{"address": server.Listener.Addr().String(), "sni": "peers.llm.test",
			"cert": filepath.Join(dir, "tls.crt"), "key": filepath.Join(dir, "tls.key"), "ca": filepath.Join(dir, "ca.crt")}})
	cmd := exec.Command(python, "-c", runner)
	cmd.Env = append(os.Environ(), "AIGW_CAPACITY="+string(config), "AIGW_WORK="+dir)
	out, _ := cmd.CombinedOutput()
	l := ParseLog(string(out))
	if l.Error == "" || l.Error[:29] != "the model takes at most 1,200" || sni != "peers.llm.test" || host != "peers.llm.test" {
		t.Fatalf("error %q, sni %q, host %q\n%s", l.Error, sni, host, out)
	}
}

// A prefill/decode measurement: every step goes through the proxy to the
// peer listener, with the site's certificate and the fleet's server name,
// and the proxy opens the connections a step needs before it starts.
func TestRunnerPeerSteps(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "aiperf"), []byte(fakeAIPerf), 0o755); err != nil {
		t.Fatal(err)
	}
	cert, pool := pki(t, dir, "peers.llm.test")
	var mu sync.Mutex
	var prompt, cached, output int64
	hosts := map[string]int{}
	var conns atomic.Int64
	gateway := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts[r.Host]++
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m","object":"model","max_model_len":200000}]}`)
		case "/served":
			var n struct{ Prompt, Cached, Output int64 }
			json.NewDecoder(r.Body).Decode(&n)
			mu.Lock()
			prompt, cached, output = prompt+n.Prompt, cached+n.Cached, output+n.Output
			mu.Unlock()
		}
	}))
	gateway.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	gateway.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	gateway.StartTLS()
	defer gateway.Close()
	// The decode pods' counters, read directly.
	pods := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "vllm:prompt_tokens_total %d\nvllm:prompt_tokens_cached_total %d\nvllm:generation_tokens_total %d\nvllm:num_requests_running 0\n",
			prompt, cached, output)
	}))
	defer pods.Close()

	set := Defaults(0)
	set.MaxConcurrency, set.BisectSteps = 4, 0
	config, _ := json.Marshal(map[string]any{"model": "m", "settings": set, "grace_seconds": 1, "agentic": Agentic(set),
		"metrics_urls": []string{pods.URL}, "wait_idle_seconds": 0, "settle_seconds": 0, "quiet_seconds": 0.1, "proxy_processes": 2,
		"peer": map[string]string{"address": gateway.Listener.Addr().String(), "sni": "peers.llm.test",
			"cert": filepath.Join(dir, "tls.crt"), "key": filepath.Join(dir, "tls.key"), "ca": filepath.Join(dir, "ca.crt")}})
	cmd := exec.Command(python, "-c", runner)
	cmd.Env = append(os.Environ(), "AIGW_CAPACITY="+string(config), "AIGW_WORK="+dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, _ := cmd.CombinedOutput()
	l := ParseLog(string(out))
	var ran []int
	for _, s := range l.Steps {
		ran = append(ran, s.Concurrency)
	}
	if !l.Done || fmt.Sprint(ran) != "[1 2 4]" {
		t.Fatalf("steps %v\n%s", ran, out)
	}
	// Each step's tokens reached the pods through the proxy, and nothing
	// else did.
	if s := l.Steps[2]; s.ServerPromptTokens == nil || *s.ServerPromptTokens != 400_000 || *s.ForeignShare(set) != 0 {
		t.Fatalf("step = %+v", s)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hosts) != 1 || hosts["peers.llm.test"] != 4 {
		t.Fatalf("hosts = %v", hosts)
	}
	// The fake AIPerf sends one request a step, so one connection would do;
	// the step at 4 had four opened before it started.
	if n := conns.Load(); n < 4 {
		t.Fatalf("%d connections to the gateway", n)
	}
}
