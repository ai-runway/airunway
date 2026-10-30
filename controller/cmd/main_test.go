package main

import (
	"bytes"
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureBootstrapCertsRegeneratesIncompletePair(t *testing.T) {
	tests := []struct {
		name        string
		existing    string
		placeholder string
	}{
		{name: "missing private key", existing: "tls.crt", placeholder: "stale certificate"},
		{name: "missing certificate", existing: "tls.key", placeholder: "stale private key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tt.existing), []byte(tt.placeholder), 0o600); err != nil {
				t.Fatalf("write incomplete bootstrap pair: %v", err)
			}

			if err := ensureBootstrapCerts(dir); err != nil {
				t.Fatalf("ensureBootstrapCerts() error = %v", err)
			}
			if _, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")); err != nil {
				t.Fatalf("generated files are not a matching TLS pair: %v", err)
			}
		})
	}
}

func TestEnsureBootstrapCertsPreservesCompletePair(t *testing.T) {
	dir := t.TempDir()
	if err := ensureBootstrapCerts(dir); err != nil {
		t.Fatalf("initial ensureBootstrapCerts() error = %v", err)
	}

	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	certBefore, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read initial certificate: %v", err)
	}
	keyBefore, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read initial private key: %v", err)
	}

	if err := ensureBootstrapCerts(dir); err != nil {
		t.Fatalf("second ensureBootstrapCerts() error = %v", err)
	}
	certAfter, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read preserved certificate: %v", err)
	}
	keyAfter, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read preserved private key: %v", err)
	}

	if !bytes.Equal(certBefore, certAfter) || !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("complete bootstrap pair was unexpectedly regenerated")
	}
}
