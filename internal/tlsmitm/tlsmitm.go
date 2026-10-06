// Package tlsmitm は、許可した行き先への TLS 接続を host 側で終端するための
// 使い捨ての CA と、その CA が署名する行き先ごとのサーバー証明書を作る。
//
// 外向き HTTPS の中身は、TLS を終端しない限りコンテンツガードから見えない。ここでは
// run ごとに使い捨ての CA を 1 つ作り、その証明書を guest の信頼ストアに入れておく。
// 透明プロキシは接続先の名前 (SNI) ごとに CA の署名した証明書を提示して TLS を
// 終端し、平文になった HTTP をコンテンツガードにかけてから、本来のサーバーへ TLS で
// 張り直す。CA の鍵は host の run の作業ディレクトリ (0700) から出ない。
package tlsmitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"
)

// 証明書の有効期間。CA は run のあいだだけ使うので短くてよい (guest の時計は KVM が
// 合わせるが、ずれても切れないよう前後に余裕を持たせる)。リーフは接続のたびに作る
// ので、CA の期限より前に切れないよう同じ長さにする。
const (
	notBeforeSkew = time.Hour
	certLifetime  = 7 * 24 * time.Hour
)

// CA は run ごとの使い捨て認証局。接続先の名前ごとにサーバー証明書を作って署名する。
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte

	mu    sync.Mutex
	leafs map[string]tls.Certificate
}

// NewCA は新しい使い捨て CA を作る。
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "quagent ephemeral CA", Organization: []string{"quagent"}},
		NotBefore:             now.Add(-notBeforeSkew),
		NotAfter:              now.Add(certLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leafs:   map[string]tls.Certificate{},
	}, nil
}

// CertPEM は CA の証明書 (PEM) を返す。guest の信頼ストアに入れるのに使う。
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// KeyPEM は CA の秘密鍵 (PKCS#8 PEM) を返す。
func (ca *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(ca.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// FromPEM は CertPEM / KeyPEM から CA を復元する (ランチャの子プロセス用)。
func FromPEM(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, errors.New("CA の証明書 (PEM) を読めない")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("CA の秘密鍵 (PEM) を読めない")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("CA の秘密鍵が ECDSA ではない")
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, leafs: map[string]tls.Certificate{}}, nil
}

// Leaf は name 用のサーバー証明書を返す。同じ名前には同じ証明書を使う。
func (ca *CA) Leaf(name string) (tls.Certificate, error) {
	if name == "" {
		return tls.Certificate{}, errors.New("名前が空")
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.leafs[name]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-notBeforeSkew),
		NotAfter:     now.Add(certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(name); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	c := tls.Certificate{
		// リーフ + CA を並べる。CA を信頼しているクライアントでも、リーフだけで
		// 足りない経路 (CA を信頼ストアに入れていない一時的なクライアント) でも
		// チェーンを組めるようにする。
		Certificate: [][]byte{der, ca.cert.Raw},
		PrivateKey:  key,
	}
	ca.leafs[name] = c
	return c, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// VerifyChain は leaf が ca で検証できるかを確かめる (テスト用)。
func VerifyChain(caPEM []byte, c tls.Certificate, name string) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("CA の証明書を読めない")
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
		return fmt.Errorf("証明書を検証できない: %w", err)
	}
	return nil
}
