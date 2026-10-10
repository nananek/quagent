package image

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"github.com/nananek/quagent/internal/paths"
)

func TestFindChecksum(t *testing.T) {
	s256 := strings.Repeat("a", 64)
	s512 := strings.Repeat("B", 128)
	list := s256 + "  other.qcow2\n" + s512 + " *arch.qcow2\n"
	if got, err := findChecksum(list, "arch.qcow2"); err != nil || got != strings.ToLower(s512) {
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
		if r.Source == "builtin" && (r.SignatureURL == "" || r.SigningKey == "") {
			t.Errorf("%s: 署名検証 (signature_url / signing_key) が無い", r.Name)
		}
	}
}

// 一覧は硬化を一番強くできる Gentoo を先頭に、Gentoo/Arch の順で出す。
func TestRecipesOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // 利用者のレシピを混ぜない
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rs, err := Recipes()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rs {
		names = append(names, r.Name)
	}
	if got := strings.Join(names, ","); got != "gentoo,arch" {
		t.Fatalf("並び: got %q", got)
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
		"verify_config /etc/kernel/config.d/zz-quagent-hardening.config",
		"KCONFIG=$(ls -1 /usr/src/linux-*/.config",
		"CONFIG_MITIGATION_PAGE_TABLE_ISOLATION=y",
		"CONFIG_MITIGATION_RETPOLINE=y",
	} {
		if !strings.Contains(r.userData, want) {
			t.Errorf("gentoo の user-data に %q が無い", want)
		}
	}
	// 6.8 で MITIGATION_ 接頭辞に改名された古い名前や、廃止・無効化できない
	// シンボルを残さない (書いても無視され、硬化が静かに効かなくなる)。注釈では
	// なく設定行だけを見る。
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
			"CONFIG_IO_URING=", // 6.18 では n にしても y に戻るので書かない
		} {
			if strings.HasPrefix(line, stale) {
				t.Errorf("gentoo の user-data に廃止・改名された設定行が残っている: %q", line)
			}
		}
	}
}

// gentoo の硬化は、攻撃面の削減と実行時の sysctl も揃っていること。
func TestGentooHardeningSettings(t *testing.T) {
	r, err := FindRecipe("gentoo")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"CONFIG_RANDOM_KMALLOC_CACHES=y",
		"CONFIG_LIST_HARDENED=y",
		"CONFIG_BPF_LSM=y",
		`CONFIG_LSM="landlock,yama,safesetid,bpf,lockdown"`,
		"CONFIG_ETHERNET=n",        // VM に出てこないハードウェアのドライバは積まない
		"CONFIG_SCSI_BNX2_ISCSI=n", // select ETHERNET で戻すので一緒に止める
		"CONFIG_USB=n",
		"CONFIG_JOYSTICK_XPAD=n", // select USB で戻すので一緒に止める
		"kernel.unprivileged_bpf_disabled=2",
		"kernel.io_uring_disabled=2", // io_uring は CONFIG では止められない
		"kernel.sysrq=0",
		"dev.tty.ldisc_autoload=0",
	} {
		if !strings.Contains(r.userData, want) {
			t.Errorf("gentoo の user-data に %q が無い", want)
		}
	}
}

// arch の硬化は、linux-hardened の導入、非特権 user namespace 許可、GRUB 起動パラメータ硬化が揃っていること。
func TestArchHardeningSettings(t *testing.T) {
	r, err := FindRecipe("arch")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"linux-hardened",
		"kernel.unprivileged_userns_clone=1",
		"slab_nomerge",
		"pacman -Rdd --noconfirm --nosave linux",
		"grub-mkconfig",
	} {
		if !strings.Contains(r.userData, want) {
			t.Errorf("arch の user-data に %q が無い", want)
		}
	}
}

// arch の焼き込みは、開始直後に serial-getty を止める。ttyS0 を getty が
// 再初期化すると焼き込みの出力や完了マーカーが消え、ビルドが失敗扱いになる
// (放置された VM が login プロンプトで止まって見えるのと同じ症状)。
func TestArchStopsSerialGetty(t *testing.T) {
	r, err := FindRecipe("arch")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.userData, "systemctl stop serial-getty@ttyS0.service") {
		t.Error("arch の user-data に serial-getty の停止が無い")
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

// 展開後の user-data に行頭から始まる不正な行が無いこと。write_files の
// リテラルブロック (`content: |`) の中に行頭の行 (ヒアドキュメントの本体や
// 終端など) があると YAML 全体が壊れ、cloud-init が何も実行せず VM が
// 放置される (ビルドがタイムアウトまで終わらない)。
func TestUserDataNoColumnZeroContent(t *testing.T) {
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
		if err := tmpl.Execute(&b, map[string]string{"User": "agent", "Marker": "QUAGENT_BUILD_OK"}); err != nil {
			t.Fatalf("%s: %v", r.Name, err)
		}
		for i, line := range strings.Split(b.String(), "\n") {
			if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
				strings.HasPrefix(line, "#") || line == "---" || line == "..." {
				continue
			}
			// トップレベルのマッピングキー (例: users:) だけが行頭に来られる。
			if !strings.Contains(line, ":") {
				t.Errorf("%s の %d 行目が行頭から始まり YAML を壊す: %q", r.Name, i+1, line)
			}
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

func TestRecipeTimeout(t *testing.T) {
	rDefault := Recipe{}
	if got := rDefault.timeout(); got != 45*time.Minute {
		t.Errorf("timeout() = %v, want 45m", got)
	}

	rCustom := Recipe{BuildTimeoutMinutes: 120}
	if got := rCustom.timeout(); got != 120*time.Minute {
		t.Errorf("timeout() = %v, want 120m", got)
	}
}

func TestFindRecipeNotFound(t *testing.T) {
	if _, err := FindRecipe("nonexistent-os-recipe"); err == nil {
		t.Fatal("expected error for nonexistent recipe")
	}
}

func TestLatestNotFound(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if _, err := Latest("gentoo"); err == nil {
		t.Fatal("expected error when no image exists")
	}
}

func TestLatestFound(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := paths.ImagesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldImg := filepath.Join(dir, "base-gentoo-20261001-100000.qcow2")
	newImg := filepath.Join(dir, "base-gentoo-20261005-100000.qcow2")
	for _, p := range []string{oldImg, newImg} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	latest, err := Latest("gentoo")
	if err != nil {
		t.Fatalf("Latest() failed: %v", err)
	}
	if latest.Path != newImg {
		t.Fatalf("Latest() = %q, want %q", latest.Path, newImg)
	}
}

func TestRemoveInvalidPath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// 1. ImagesDir の外
	if err := Remove(Image{Path: "/etc/passwd"}); err == nil {
		t.Fatal("expected error when removing path outside ImagesDir")
	}

	// 2. 命名規則に一致しない
	badName := filepath.Join(paths.ImagesDir(), "some-random-file.txt")
	if err := Remove(Image{Path: badName}); err == nil {
		t.Fatal("expected error when removing file with invalid name format")
	}
}

func TestFetchSmall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("hello world"))
		case "/notfound":
			w.WriteHeader(http.StatusNotFound)
		case "/big":
			w.Write(bytes.Repeat([]byte("A"), 100))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	// 1. 正常取得
	b, err := fetchSmall(srv.URL+"/ok", 64)
	if err != nil || string(b) != "hello world" {
		t.Fatalf("fetchSmall ok: got %q, %v", string(b), err)
	}

	// 2. 404 エラー
	if _, err := fetchSmall(srv.URL+"/notfound", 64); err == nil {
		t.Fatal("expected error on 404")
	}

	// 3. サイズ超過
	if _, err := fetchSmall(srv.URL+"/big", 50); err == nil {
		t.Fatal("expected error on size limit exceeded")
	}
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("image content for checksum test")
	h := sha256.Sum256(data)
	sum256 := hex.EncodeToString(h[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sum256 + "  test.qcow2\n"))
	}))
	defer srv.Close()

	r := Recipe{
		CloudImageURL: "https://example.com/test.qcow2",
		ChecksumURL:   srv.URL + "/checksums.sha256",
	}

	// 1. 一致
	if err := verifyChecksum(r, sum256, ""); err != nil {
		t.Fatalf("verifyChecksum failed on matching hash: %v", err)
	}

	// 2. 不一致
	wrongHash := strings.Repeat("0", 64)
	if err := verifyChecksum(r, wrongHash, ""); err == nil {
		t.Fatal("expected error on mismatched hash")
	}
}

func TestVerifyFile(t *testing.T) {
	tmp := t.TempDir()
	filePath := filepath.Join(tmp, "test.qcow2")
	content := []byte("fake qcow2 image content")
	if err := os.WriteFile(filePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(content)
	sum256 := hex.EncodeToString(h[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sum256 + "  test.qcow2\n"))
	}))
	defer srv.Close()

	r := Recipe{
		CloudImageURL: "https://example.com/test.qcow2",
		ChecksumURL:   srv.URL + "/checksums.sha256",
	}

	// 1. 正常系
	if err := verifyFile(r, filePath); err != nil {
		t.Fatalf("verifyFile failed: %v", err)
	}

	// 2. 存在しないファイル
	if err := verifyFile(r, filepath.Join(tmp, "nonexistent.qcow2")); err == nil {
		t.Fatal("expected error for nonexistent file")
	}

	// 3. ハッシュ不一致
	rBad := Recipe{
		CloudImageURL: "https://example.com/bad.qcow2",
		ChecksumURL:   srv.URL + "/checksums.sha256",
	}
	badFile := filepath.Join(tmp, "bad.qcow2")
	if err := os.WriteFile(badFile, []byte("different content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyFile(rBad, badFile); err == nil {
		t.Fatal("expected error for bad file")
	}
}

func TestRunWithTimeout(t *testing.T) {
	// 1. 正常終了
	cmd := exec.Command("echo", "hello")
	out, err := runWithTimeout(cmd, 2*time.Second)
	if err != nil || !strings.Contains(string(out), "hello") {
		t.Fatalf("runWithTimeout failed: out=%q, err=%v", string(out), err)
	}

	// 2. タイムアウト
	cmdSlow := exec.Command("sleep", "2")
	_, err = runWithTimeout(cmdSlow, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "タイムアウト") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestBuildErrors(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// 1. 差分更新 (Incremental) でベースイメージが無い
	r := Recipe{Name: "gentoo"}
	var buf bytes.Buffer
	_, err := Build(r, BuildOpts{Incremental: true}, &buf)
	if err == nil || !strings.Contains(err.Error(), "差分更新には前回のイメージが要る") {
		t.Fatalf("expected incremental error, got %v", err)
	}

	// 2. resolveLatest が失敗する
	rFail := Recipe{
		Name:          "custom",
		LatestURL:     "http://127.0.0.1:0/latest",
		CloudImageURL: "https://example.com/image-{{latest}}.qcow2",
	}
	_, err = Build(rFail, BuildOpts{Incremental: false}, &buf)
	if err == nil {
		t.Fatal("expected error on failed resolveLatest")
	}
}

func TestReadRecipeInvalid(t *testing.T) {
	// 1. recipe.json が存在しない
	fsysNoRecipe := fstest.MapFS{
		"user-data.yaml": &fstest.MapFile{Data: []byte("#cloud-config\n")},
	}
	if _, err := readRecipe(fsysNoRecipe, "dir", "test"); err == nil {
		t.Fatal("expected error when recipe.json is missing")
	}

	// 2. 不正な JSON
	fsysBadJSON := fstest.MapFS{
		"recipe.json":    &fstest.MapFile{Data: []byte("not valid json")},
		"user-data.yaml": &fstest.MapFile{Data: []byte("#cloud-config\n")},
	}
	if _, err := readRecipe(fsysBadJSON, "dir", "test"); err == nil {
		t.Fatal("expected error on invalid recipe.json")
	}

	// 3. user-data.yaml が存在しない
	fsysNoUserData := fstest.MapFS{
		"recipe.json": &fstest.MapFile{Data: []byte(`{"cloud_image_url":"https://example.com/img.qcow2"}`)},
	}
	if _, err := readRecipe(fsysNoUserData, "dir", "test"); err == nil {
		t.Fatal("expected error when user-data.yaml is missing")
	}
}

func TestRecipesWithUserDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	recipesDir := RecipesDir()
	userRecipeDir := filepath.Join(recipesDir, "mycustom")
	if err := os.MkdirAll(userRecipeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// ディレクトリ以外のファイルも置いてみる (!e.IsDir() のカバレッジ)
	if err := os.WriteFile(filepath.Join(recipesDir, "ignored.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	// ユーザー定義レシピを作成
	recipeJSON := `{"cloud_image_url":"https://example.com/custom.qcow2","checksum_url":"https://example.com/custom.sha256"}`
	if err := os.WriteFile(filepath.Join(userRecipeDir, "recipe.json"), []byte(recipeJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userRecipeDir, "user-data.yaml"), []byte("#cloud-config\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, err := Recipes()
	if err != nil {
		t.Fatalf("Recipes() failed: %v", err)
	}
	found := false
	for _, r := range rs {
		if r.Name == "mycustom" {
			found = true
			if r.Source != userRecipeDir {
				t.Errorf("Source = %q, want %q", r.Source, userRecipeDir)
			}
		}
	}
	if !found {
		t.Fatal("user recipe 'mycustom' was not found in Recipes()")
	}

	// FindRecipe でも見つかる
	r, err := FindRecipe("mycustom")
	if err != nil || r.Name != "mycustom" {
		t.Fatalf("FindRecipe(mycustom) failed: %v", err)
	}
}
