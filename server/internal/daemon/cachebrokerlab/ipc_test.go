package cachebrokerlab

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIPCRejectsUnauthenticatedClientAndTaskLifecycleFlags(t *testing.T) {
	dir := t.TempDir()
	if err := CreateLabIdentity(dir); err != nil {
		t.Fatal(err)
	}
	s := serviceFixture(t, t.TempDir())
	server := httptest.NewUnstartedServer(s)
	var err error
	server.TLS, err = tlsConfig(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	defer server.Close()
	trusted, err := tlsConfig(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: trusted}
	defer transport.CloseIdleConnections()
	client := &Client{Endpoint: server.URL, HTTP: &http.Client{Transport: transport}}
	resp, err := client.Call(context.Background(), Request{Operation: "hello"})
	if err != nil || resp.Epoch != s.epoch {
		t.Fatalf("trusted daemon could not authenticate: %+v %v", resp, err)
	}
	for _, mode := range []string{"no-cert", "wrong-cert"} {
		t.Run(mode, func(t *testing.T) {
			cfg := trusted.Clone()
			cfg.Certificates = nil
			if mode == "wrong-cert" {
				cfg.Certificates = server.TLS.Certificates
			}
			tr := &http.Transport{TLSClientConfig: cfg}
			defer tr.CloseIdleConnections()
			attacker := &Client{Endpoint: server.URL, HTTP: &http.Client{Transport: tr}}
			if _, err := attacker.Call(context.Background(), Request{Operation: "register", Epoch: s.epoch, RequestID: "attack", RecipeID: "pinned-fixture"}); err == nil {
				t.Fatal("ordinary caller acquired authority")
			}
		})
	}
	req, _ := http.NewRequest("POST", server.URL+"/v1", bytes.NewBufferString(`{"Operation":"end","Terminal":true,"Active":false,"Path":"/tmp"}`))
	res, err := client.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || len(s.journal.Homes) != 0 {
		t.Fatal("task-supplied lifecycle/path accepted")
	}

	other := t.TempDir()
	if err := CreateLabIdentity(other); err != nil {
		t.Fatal(err)
	}
	forged := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("sent request to forged endpoint") }))
	forged.TLS, err = tlsConfig(other, true)
	if err != nil {
		t.Fatal(err)
	}
	forged.StartTLS()
	defer forged.Close()
	client.Endpoint = forged.URL
	if _, err := client.Call(context.Background(), Request{Operation: "hello"}); err == nil {
		t.Fatal("forged endpoint trusted")
	}
}

// A certificate issued by the same CA is not sufficient: exact daemon identity
// is pinned in addition to normal TLS chain and usage verification.
func TestTLSPinsExactClientCertificate(t *testing.T) {
	dir := t.TempDir()
	if err := CreateLabIdentity(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsConfig(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := tlsConfig(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer.Certificates[0].Leaf}}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cfg.Certificates[0].Leaf}}); err == nil {
		t.Fatal("authorized CA treated as exact daemon credential")
	}
}
