package image

import (
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
