// tlsServe.go — throwaway TLS archive server for the KB-279 E30 fixture.
//
// The criteria engine refuses to fetch plain HTTP archive sources, real ACME
// certs are out of scope for an offline suite, and openssl is not on the
// runner — so this tiny Go program mints a throwaway self-signed cert at
// startup, writes its PEM for the client to trust through SSL_CERT_FILE (Go's
// x509 honors it), serves ONE archive over loopback TLS, and exits when the
// parent kills it. Nothing else: no persistence, no reuse, no real
// credentials.
//
// Stdlib only. The standalone suite compiles and runs it per invocation.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	certOut := flag.String("certout", "/tmp/criteria-e30-serve-cert.pem", "write the throwaway self-signed cert PEM here for SSL_CERT_FILE trust")
	port := flag.String("port", "8091", "listen port on 127.0.0.1")
	served := flag.String("file", "", "archive file to serve (required)")
	flag.Parse()
	if *served == "" {
		log.Fatal("tlsServe: -file is required (the archive to serve)")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "criteria-e30-throwaway"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*certOut, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		log.Fatal(err)
	}

	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	srv := &http.Server{
		Addr:      "127.0.0.1:" + *port,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("tlsServe: GET %s", r.URL.Path)
			http.ServeFile(w, r, *served)
		}),
	}
	log.Printf("tlsServe: serving %s on https://127.0.0.1:%s (cert trusted via SSL_CERT_FILE=%s)", *served, *port, *certOut)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
