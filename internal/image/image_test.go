package image

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"text/template"

	"github.com/nananek/quagent/internal/paths"
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

// firmware は uefi / bios だけ受け付ける (既定は bios)。
func TestRecipeFirmware(t *testing.T) {
	fsys := func(firmware string) fstest.MapFS {
		return fstest.MapFS{
			"recipe.json": &fstest.MapFile{Data: []byte(
				`{"cloud_image_url":"u","checksum_url":"c","firmware":"` + firmware + `"}`)},
			"user-data.yaml": &fstest.MapFile{Data: []byte("#cloud-config\n")},
		}
	}
	if r, err := readRecipe(fsys("uefi"), "t", "test"); err != nil || r.Firmware != "uefi" {
		t.Fatalf("uefi: got %q, %v", r.Firmware, err)
	}
	if r, err := readRecipe(fsys(""), "t", "test"); err != nil || r.Firmware != "" {
		t.Fatalf("既定: got %q, %v", r.Firmware, err)
	}
	if r, err := readRecipe(fsys("bios"), "t", "test"); err != nil || r.Firmware != "bios" {
		t.Fatalf("bios: got %q, %v", r.Firmware, err)
	}
	if _, err := readRecipe(fsys("legacy"), "t", "test"); err == nil {
		t.Fatal("不明な firmware を受け付けた")
	}
}

// Gentoo は UEFI 専用のクラウドイメージなので firmware を uefi にする。
func TestGentooRecipeIsUEFI(t *testing.T) {
	r, err := FindRecipe("gentoo")
	if err != nil {
		t.Fatal(err)
	}
	if r.Firmware != "uefi" {
		t.Fatalf("got %q", r.Firmware)
	}
}

// gentoo の焼き込みは、要求した硬化 CONFIG が実際に効いたかを検証してから成功と
// する (シンボルの改名・廃止で黙って効かなくなるのを防ぐ)。改名済み・廃止済みの
// シンボルに戻っていないことも確かめる。
func TestGentooVerifiesHardeningConfig(t *testing.T) {
	r, err := FindRecipe("gentoo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"verify_config /etc/kernel/config.d/50-quagent-docker.config",
		"verify_config /etc/kernel/config.d/90-quagent-hardening.config",
		"CONFIG_MITIGATION_PAGE_TABLE_ISOLATION=y",
		"CONFIG_MITIGATION_RETPOLINE=y",
	} {
		if !strings.Contains(r.userData, want) {
			t.Errorf("gentoo の user-data に %q が無い", want)
		}
	}
	// 6.8 で MITIGATION_ 接頭辞に改名された古い名前や、廃止されたシンボルを残さない
	// (書いても無視され、硬化が静かに効かなくなる)。注釈ではなく設定行だけを見る。
	for _, line := range strings.Split(r.userData, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "CONFIG_") {
			continue
		}
		for _, stale := range []string{
			"CONFIG_PAGE_TABLE_ISOLATION=",
			"CONFIG_RETPOLINE=",
			"CONFIG_BPF_JIT_HARDEN=",
			"CONFIG_DEVKMEM",
			"CONFIG_ACPI_CUSTOM_METHOD",
			"CONFIG_X86_X32=",
		} {
			if strings.HasPrefix(line, stale) {
				t.Errorf("gentoo の user-data に廃止・改名された設定行が残っている: %q", line)
			}
		}
	}
}

// 差分更新なのに前回のイメージが無ければ、何も焼かずにエラーにする。
func TestBuildIncrementalNeedsPrevious(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	r := Recipe{Name: "gentoo", CloudImageURL: "https://example.invalid/x.qcow2",
		ChecksumURL: "https://example.invalid/x.qcow2.sha256"}
	if _, err := Build(r, BuildOpts{CPUs: 1, MemMiB: 256, Incremental: true}, io.Discard); err == nil {
		t.Fatal("前回のイメージが無いのに成功した")
	}
}

// どのレシピの user-data も、quagent が埋める値 (User・Marker) で描ける。
func TestUserDataTemplates(t *testing.T) {
	rs, err := Recipes()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		tmpl, err := template.New("user-data").Parse(r.userData)
		if err != nil {
			t.Fatalf("%s: %v", r.Name, err)
		}
		var b bytes.Buffer
		err = tmpl.Execute(&b, map[string]string{"User": "agent", "Marker": "QUAGENT_BUILD_OK"})
		if err != nil {
			t.Fatalf("%s: %v", r.Name, err)
		}
	}
}

// 付帯情報は既定 (bios) では書かず、uefi のときだけ書いて読める。
func TestImageMeta(t *testing.T) {
	dir := t.TempDir()
	uefi := filepath.Join(dir, "base-gentoo-20261006-120000.qcow2")
	if err := writeMeta(uefi, imageMeta{Firmware: "uefi"}); err != nil {
		t.Fatal(err)
	}
	if got := readMeta(uefi).Firmware; got != "uefi" {
		t.Fatalf("got %q", got)
	}
	bios := filepath.Join(dir, "base-arch-20261006-120000.qcow2")
	if err := writeMeta(bios, imageMeta{Firmware: "bios"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(metaPath(bios)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("既定 (bios) で付帯情報を書いた")
	}
	if got := readMeta(bios).Firmware; got != "" {
		t.Fatalf("既定が空でない: %q", got)
	}
}

// List は付帯情報の firmware を拾い、Remove はイメージと一緒に消す。
func TestListAndRemoveWithMeta(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := paths.ImagesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uefi := filepath.Join(dir, "base-gentoo-20261006-120000.qcow2")
	bios := filepath.Join(dir, "base-arch-20261006-115959.qcow2")
	for _, p := range []string{uefi, bios} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeMeta(uefi, imageMeta{Firmware: "uefi"}); err != nil {
		t.Fatal(err)
	}
	imgs, err := List("")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, img := range imgs {
		got[filepath.Base(img.Path)] = img.Firmware
	}
	if got[filepath.Base(uefi)] != "uefi" || got[filepath.Base(bios)] != "" {
		t.Fatalf("firmware が拾えていない: %v", got)
	}
	if err := Remove(Image{Path: uefi}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{uefi, metaPath(uefi)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s が残っている", p)
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
