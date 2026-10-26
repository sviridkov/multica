package cachebrokerlab

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// CreateLabIdentity generates ephemeral lab credentials, never production ones.
// The privileged bootstrap must first install dir with a private native ACL.
// The CA key is not persisted; rotation requires explicit new bootstrap.
func CreateLabIdentity(dir string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable cache lab"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "ca.pem"), "CERTIFICATE", caDER); err != nil {
		return err
	}
	for i, name := range []string{"server", "client"} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: name}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature}
		if name == "server" {
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			leaf.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		} else {
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, name+".pem"), "CERTIFICATE", der); err != nil {
			return err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, name+".key"), "PRIVATE KEY", keyDER); err != nil {
			return err
		}
	}
	return nil
}

func writePEM(path, kind string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = pem.Encode(f, &pem.Block{Type: kind, Bytes: data})
	return errors.Join(err, f.Sync(), f.Close())
}

func ServiceTLS(dir string, server bool) (*tls.Config, error) {
	if err := privateCredential(dir); err != nil {
		return nil, err
	}
	name := "client"
	if server {
		name = "server"
	}
	if err := privateCredential(filepath.Join(dir, name+".key")); err != nil {
		return nil, err
	}
	return tlsConfig(dir, server)
}

func tlsConfig(dir string, server bool) (*tls.Config, error) {
	name, peer := "client", "server"
	if server {
		name, peer = "server", "client"
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key"))
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid pinned CA")
	}
	data, err := os.ReadFile(filepath.Join(dir, peer+".pem"))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("invalid pinned peer")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: roots}
	if server {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = roots
	}
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) != 1 || !bytes.Equal(state.PeerCertificates[0].Raw, block.Bytes) {
			return errors.New("peer is not the pinned authority")
		}
		return nil
	}
	return cfg, nil
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		http.Error(w, "authenticated TLS required", http.StatusUnauthorized)
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1" {
		http.Error(w, "unknown method", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	data, err := io.ReadAll(r.Body)
	var req Request
	if err == nil {
		err = strictJSON(data, &req)
	}
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	resp := s.Execute(r.Context(), req)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	} // Outcome is durable; client recovers by request ID.
}

type Endpoint struct {
	Address, Epoch string
	PID            int
}
type Client struct {
	Endpoint string
	HTTP     *http.Client
}

func NewClient(control string) (*Client, error) {
	if err := privateCredential(control); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(control, "endpoint.json"))
	if err != nil {
		return nil, err
	}
	var endpoint Endpoint
	if err := strictJSON(data, &endpoint); err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host != "127.0.0.1" {
		return nil, errors.New("non-loopback endpoint denied")
	}
	cfg, err := ServiceTLS(control, false)
	if err != nil {
		return nil, err
	}
	return &Client{Endpoint: "https://" + endpoint.Address, HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 2 * time.Minute}}, nil
}
func (c *Client) Close() { c.HTTP.CloseIdleConnections() }
func (c *Client) Call(ctx context.Context, req Request) (Response, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	r, err := http.NewRequestWithContext(ctx, "POST", c.Endpoint+"/v1", bytes.NewReader(data))
	if err != nil {
		return Response{}, err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("IPC status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, err
	}
	var result Response
	err = strictJSON(body, &result)
	return result, err
}

// ServeService binds only loopback and publishes readiness in the protected
// control directory. The caller holds the kernel journal lock until it exits.
func ServeService(ctx context.Context, s *Service, control string) error {
	cfg, err := ServiceTLS(control, true)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	endpoint, _ := json.Marshal(Endpoint{Address: listener.Addr().String(), Epoch: s.epoch, PID: os.Getpid()})
	temp := filepath.Join(control, "endpoint-"+nonce()+".tmp")
	if err := os.WriteFile(temp, endpoint, 0600); err != nil {
		return err
	}
	if err := replaceServiceFile(temp, filepath.Join(control, "endpoint.json")); err != nil {
		return err
	}
	server := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			server.Close()
		case <-done:
		}
	}()
	err = server.Serve(tls.NewListener(listener, cfg))
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
