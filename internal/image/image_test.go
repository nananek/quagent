package image

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindChecksum(t *testing.T) {
	s256 := strings.Repeat("a", 64)
	s512 := strings.Repeat("B", 128)
	list := s256 + "  other.qcow2\n" + s512 + " *debian.qcow2\n"
	if got, err := findChecksum(list, "debian.qcow2"); err != nil || got != strings.ToLower(s512) {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := findChecksum(list, "missing.qcow2"); err == nil {
		t.Fatal("無い名前で成功した")
	}
	if _, err := findChecksum("xyz  a.qcow2\n", "a.qcow2"); err == nil {
		t.Fatal("16 進でないチェックサムを受け付けた")
	}
}

func TestBuiltinRecipesAreVerifiable(t *testing.T) {
	rs, err := Recipes()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Source == "builtin" && r.ChecksumURL == "" {
			t.Errorf("%s: checksum_url が無い", r.Name)
		}
	}
}

// latest は署名されたテキスト。clearsigned のヘッダや注釈を飛ばしてファイル名を取る。
func TestLatestFileName(t *testing.T) {
	const signed = `-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA256

# Latest as of Tue, 06 Oct 2026 10:15:00 +0000
# ts=1791281700
di-amd64-cloudinit-20261004T164559Z.qcow2 1501626368
-----BEGIN PGP SIGNATURE-----

iQFPBAEBCAA5...
-----END PGP SIGNATURE-----
`
	if got, err := latestFileName(signed); err != nil || got != "di-amd64-cloudinit-20261004T164559Z.qcow2" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := latestFileName("# comment only\nHash: SHA256\n"); err == nil {
		t.Fatal("ファイル名が無いのに成功した")
	}
}

// LatestURL の指すファイルから今の名前を読み、URL 中の $FILE を置き換える。
func TestResolveLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest.txt" {
			io.WriteString(w, "# c\nimage-20260101.qcow2 123\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	r := Recipe{
		LatestURL:     srv.URL + "/latest.txt",
		CloudImageURL: srv.URL + "/img/$FILE",
		ChecksumURL:   srv.URL + "/img/$FILE.sha256",
		SignatureURL:  srv.URL + "/img/$FILE.asc",
	}
	var out strings.Builder
	if err := r.resolveLatest(&out); err != nil {
		t.Fatal(err)
	}
	if r.CloudImageURL != srv.URL+"/img/image-20260101.qcow2" ||
		r.ChecksumURL != srv.URL+"/img/image-20260101.qcow2.sha256" ||
		r.SignatureURL != srv.URL+"/img/image-20260101.qcow2.asc" {
		t.Fatalf("URL を置き換えていない: %+v", r)
	}
	if !strings.Contains(out.String(), "image-20260101.qcow2") {
		t.Fatalf("進捗に名前が無い: %q", out.String())
	}

	bad := Recipe{LatestURL: srv.URL + "/nope"}
	if err := bad.resolveLatest(&out); err == nil {
		t.Fatal("latest を取得できないのに成功した")
	}
}

func TestVerifySignature(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg が無い")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	gpg := func(args ...string) []byte {
		t.Helper()
		out, err := exec.Command("gpg", append([]string{"--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...).Output()
		if err != nil {
			t.Fatalf("gpg %v: %v", args, err)
		}
		return out
	}
	gpg("--quick-gen-key", "test <t@example>", "ed25519", "sign", "1d")
	key := gpg("--armor", "--export", "t@example")
	image := filepath.Join(dir, "image")
	if err := os.WriteFile(image, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	gpg("--detach-sign", "-o", image+".sig", image)

	verify := func() error {
		sig, _ := os.ReadFile(image + ".sig")
		return verifySignatureWith(Recipe{SigningKey: "k", signingKey: key}, sig, image)
	}
	if err := verify(); err != nil {
		t.Fatalf("正しい署名が通らない: %v", err)
	}
	if err := os.WriteFile(image, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verify(); err == nil {
		t.Fatal("書き換えたイメージの署名が通った")
	}
}
