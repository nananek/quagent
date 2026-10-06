package tlsmitm

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestLeafSignedByCA(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Leaf("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChain(ca.CertPEM(), c, "example.com"); err != nil {
		t.Fatal(err)
	}
	// 別の名前では検証できない (取り違えを防ぐ)
	if err := VerifyChain(ca.CertPEM(), c, "other.example"); err == nil {
		t.Fatal("別の名前で検証が通ってしまった")
	}
	// リーフ + CA のチェーンになっている
	if len(c.Certificate) != 2 {
		t.Fatalf("チェーンの長さ = %d, want 2", len(c.Certificate))
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "example.com" {
		t.Fatalf("DNSNames = %v", leaf.DNSNames)
	}
}

func TestLeafForIPSAN(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ca.Leaf("203.0.113.7")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "203.0.113.7" {
		t.Fatalf("IPAddresses = %v", leaf.IPAddresses)
	}
}

func TestLeafCached(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	a, err := ca.Leaf("example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ca.Leaf("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Certificate[0]) != string(b.Certificate[0]) {
		t.Fatal("同じ名前で別の証明書ができた")
	}
}

func TestFromPEM(t *testing.T) {
	ca, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ca.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromPEM(ca.CertPEM(), key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := got.Leaf("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChain(ca.CertPEM(), c, "example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestLeafUsableInTLS(t *testing.T) {
	// tls.Certificate がそのまま Handshake に使える形か (PrivateKey が載っているか)
	ca, _ := NewCA()
	c, err := ca.Leaf("example.com")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{c}, NextProtos: []string{"http/1.1"}}
	if cfg.Certificates[0].PrivateKey == nil {
		t.Fatal("PrivateKey が無い")
	}
}
