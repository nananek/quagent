// Package image はベースイメージを焼き、管理する。
//
// OS ごとの手順はレシピ (recipe.json + user-data.yaml) として独立させてあり、
// 組み込みのもの (recipes/<名前>/) と利用者のもの (~/.config/quagent/images/<名前>/、
// 同名なら組み込みより優先) を使える。焼くときだけ host の網をそのまま使う
// (信頼できる工程)。実行時の VM はイメージの overlay で起動する。
package image

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/nananek/quagent/internal/paths"
	"github.com/nananek/quagent/internal/vm"
)

//go:embed recipes
var builtin embed.FS

const (
	diskSize      = "40G"
	buildOKMarker = "QUAGENT_BUILD_OK"
	// DefaultRecipe は特に指定がないときのレシピ。
	DefaultRecipe = "debian"
)

// Recipe は 1 つの OS のベースイメージの作り方。
type Recipe struct {
	Name string `json:"-"`
	// Description はメニューに出す短い説明 (OS 名と版くらい。中身の詳細は書かない)。
	Description string `json:"description"`
	// Details はイメージ管理の画面に出す中身の説明 (何をどこから入れるか)。
	Details       string `json:"details,omitempty"`
	CloudImageURL string `json:"cloud_image_url"`
	// ChecksumURL は配布元のチェックサムの一覧 ("<16進> <ファイル名>" の行。sha256 か
	// sha512)。取得したイメージをこれと照合する (必須)。
	ChecksumURL string `json:"checksum_url"`
	// SignatureURL はイメージそのものへの OpenPGP の分離署名。あれば SigningKey
	// (レシピのディレクトリにある公開鍵のファイル名) で gpgv により検証する。
	SignatureURL string `json:"signature_url,omitempty"`
	SigningKey   string `json:"signing_key,omitempty"`
	// Source は "builtin" か、利用者のレシピのディレクトリ。
	Source     string `json:"-"`
	userData   string
	signingKey []byte
}

// RecipesDir は利用者のレシピの置き場。
func RecipesDir() string { return filepath.Join(filepath.Dir(paths.ConfigFile()), "images") }

func readRecipe(fsys fs.FS, name, source string) (Recipe, error) {
	b, err := fs.ReadFile(fsys, "recipe.json")
	if err != nil {
		return Recipe{}, err
	}
	var r Recipe
	if err := json.Unmarshal(b, &r); err != nil {
		return Recipe{}, fmt.Errorf("%s/recipe.json: %w", source, err)
	}
	ud, err := fs.ReadFile(fsys, "user-data.yaml")
	if err != nil {
		return Recipe{}, err
	}
	if r.CloudImageURL == "" {
		return Recipe{}, fmt.Errorf("%s: cloud_image_url が空", source)
	}
	if r.ChecksumURL == "" {
		return Recipe{}, fmt.Errorf("%s: checksum_url が空 (取得したイメージを検証できない)", source)
	}
	if (r.SignatureURL == "") != (r.SigningKey == "") {
		return Recipe{}, fmt.Errorf("%s: signature_url と signing_key は両方指定する", source)
	}
	if r.SigningKey != "" {
		if r.signingKey, err = fs.ReadFile(fsys, r.SigningKey); err != nil {
			return Recipe{}, err
		}
	}
	r.Name, r.Source, r.userData = name, source, string(ud)
	return r, nil
}

// Recipes は使えるレシピを名前順に返す。
func Recipes() ([]Recipe, error) {
	byName := map[string]Recipe{}
	entries, err := fs.ReadDir(builtin, "recipes")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		sub, _ := fs.Sub(builtin, "recipes/"+e.Name())
		r, err := readRecipe(sub, e.Name(), "builtin")
		if err != nil {
			return nil, err
		}
		byName[r.Name] = r
	}
	user, err := os.ReadDir(RecipesDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range user {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(RecipesDir(), e.Name())
		r, err := readRecipe(os.DirFS(dir), e.Name(), dir)
		if err != nil {
			return nil, err
		}
		byName[r.Name] = r
	}
	out := make([]Recipe, 0, len(byName))
	for _, r := range byName {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FindRecipe は名前でレシピを探す。
func FindRecipe(name string) (Recipe, error) {
	rs, err := Recipes()
	if err != nil {
		return Recipe{}, err
	}
	var names []string
	for _, r := range rs {
		if r.Name == name {
			return r, nil
		}
		names = append(names, r.Name)
	}
	return Recipe{}, fmt.Errorf("レシピ %q が無い (使えるもの: %s)", name, strings.Join(names, ", "))
}

// Image は焼いたベースイメージ。
type Image struct {
	Recipe string
	Path   string
	Built  time.Time
	Size   int64
}

var imageName = regexp.MustCompile(`^base-([a-z0-9][a-z0-9_-]*)-(\d{8}-\d{6})\.qcow2$`)

// List は焼いたイメージを新しい順に返す。recipe が空なら全部。
func List(recipe string) ([]Image, error) {
	entries, err := os.ReadDir(paths.ImagesDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Image
	for _, e := range entries {
		m := imageName.FindStringSubmatch(e.Name())
		if m == nil || (recipe != "" && m[1] != recipe) {
			continue
		}
		built, _ := time.ParseInLocation("20060102-150405", m[2], time.Local)
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Image{Recipe: m[1], Path: filepath.Join(paths.ImagesDir(), e.Name()), Built: built, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Built.After(out[j].Built) })
	return out, nil
}

// Latest はレシピの最新イメージを返す。
func Latest(recipe string) (Image, error) {
	imgs, err := List(recipe)
	if err != nil {
		return Image{}, err
	}
	if len(imgs) == 0 {
		return Image{}, fmt.Errorf("%s のベースイメージが無い。先に `quagent image build %s` を実行する", recipe, recipe)
	}
	return imgs[0], nil
}

// Remove は焼いたイメージを消す。
func Remove(img Image) error {
	if filepath.Dir(img.Path) != paths.ImagesDir() || !imageName.MatchString(filepath.Base(img.Path)) {
		return fmt.Errorf("quagent のイメージではない: %s", img.Path)
	}
	return os.Remove(img.Path)
}

// BuildOpts は焼き込みの設定。
type BuildOpts struct {
	CPUs   int
	MemMiB int
	// Refresh はクラウドイメージを取り直す (OS の更新を取り込む)。
	Refresh bool
}

// Build はレシピからベースイメージを新しく焼き、そのイメージを返す。
func Build(r Recipe, o BuildOpts, progress io.Writer) (Image, error) {
	var ud bytes.Buffer
	tmpl, err := template.New("user-data").Parse(r.userData)
	if err != nil {
		return Image{}, fmt.Errorf("%s の user-data.yaml: %w", r.Name, err)
	}
	if err := tmpl.Execute(&ud, map[string]string{"User": vm.GuestUser, "Marker": buildOKMarker}); err != nil {
		return Image{}, err
	}
	cloud, err := fetchCloudImage(r, o.Refresh, progress)
	if err != nil {
		return Image{}, err
	}
	if err := os.MkdirAll(paths.ImagesDir(), 0o755); err != nil {
		return Image{}, err
	}
	work, err := os.MkdirTemp(paths.ImagesDir(), "build-")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(work)

	disk := filepath.Join(work, "disk.qcow2")
	if out, err := exec.Command("qemu-img", "convert", "-O", "qcow2", cloud, disk).CombinedOutput(); err != nil {
		return Image{}, fmt.Errorf("イメージの複製に失敗: %v: %s", err, out)
	}
	if out, err := exec.Command("qemu-img", "resize", "-q", disk, diskSize).CombinedOutput(); err != nil {
		return Image{}, fmt.Errorf("リサイズに失敗: %v: %s", err, out)
	}
	stamp := time.Now().Format("20060102-150405")
	seed, err := vm.MakeSeed(work, "quagent-build-"+stamp, "quagent-build", ud.String(), nil)
	if err != nil {
		return Image{}, err
	}

	console := filepath.Join(work, "console.log")
	argv := vm.QemuArgv(vm.QemuOpts{
		Disk: disk, Seed: seed, CPUs: o.CPUs, MemMiB: o.MemMiB, ConsoleLog: console,
	})
	fmt.Fprintf(progress, "%s を VM で焼き込み中 (数分かかる)。コンソール: %s\n", r.Name, console)
	cmd := exec.Command(argv[0], argv[1:]...)
	if out, err := runWithTimeout(cmd, 45*time.Minute); err != nil {
		return Image{}, fmt.Errorf("焼き込み VM が異常終了: %v: %s", err, out)
	}

	log, _ := os.ReadFile(console)
	if !strings.Contains(string(log), buildOKMarker) {
		keep := filepath.Join(paths.ImagesDir(), "failed-"+r.Name+"-"+stamp+"-console.log")
		_ = os.WriteFile(keep, log, 0o644)
		return Image{}, fmt.Errorf("焼き込みに失敗した。コンソールログ: %s", keep)
	}

	// 履歴を潰して単独で使える形にする
	out := filepath.Join(paths.ImagesDir(), "base-"+r.Name+"-"+stamp+".qcow2")
	if b, err := exec.Command("qemu-img", "convert", "-O", "qcow2", disk, out).CombinedOutput(); err != nil {
		return Image{}, fmt.Errorf("イメージの書き出しに失敗: %v: %s", err, b)
	}
	built, _ := time.ParseInLocation("20060102-150405", stamp, time.Local)
	info, _ := os.Stat(out)
	img := Image{Recipe: r.Name, Path: out, Built: built}
	if info != nil {
		img.Size = info.Size()
	}
	return img, nil
}

func fetchCloudImage(r Recipe, refresh bool, progress io.Writer) (string, error) {
	dst := filepath.Join(paths.CacheDir(), r.Name+"-"+filepath.Base(r.CloudImageURL))
	if _, err := os.Stat(dst); err == nil && !refresh {
		return dst, nil
	}
	if err := os.MkdirAll(paths.CacheDir(), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(progress, "クラウドイメージを取得: %s\n", r.CloudImageURL)
	resp, err := http.Get(r.CloudImageURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("取得に失敗: %s", resp.Status)
	}
	part := dst + ".part"
	f, err := os.Create(part)
	if err != nil {
		return "", err
	}
	defer os.Remove(part) // 検証に通れば rename 済みで消えない
	h256, h512 := sha256.New(), sha512.New()
	if _, err := io.Copy(io.MultiWriter(f, h256, h512), resp.Body); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := verifyChecksum(r, hex.EncodeToString(h256.Sum(nil)), hex.EncodeToString(h512.Sum(nil))); err != nil {
		return "", err
	}
	if r.SignatureURL != "" {
		if err := verifySignature(r, part); err != nil {
			return "", err
		}
		fmt.Fprintf(progress, "署名を検証した (%s)\n", r.SigningKey)
	}
	return dst, os.Rename(part, dst)
}

// fetchSmall は url の中身を max バイトまで取得する (チェックサムや署名の小さなファイル)。
func fetchSmall(url string, max int64) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s の取得に失敗: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s が大きすぎる", url)
	}
	return b, nil
}

// verifyChecksum は配布元のチェックサムの一覧から、イメージのファイル名の行を探して照合する。
func verifyChecksum(r Recipe, sum256, sum512 string) error {
	list, err := fetchSmall(r.ChecksumURL, 1<<20)
	if err != nil {
		return err
	}
	want, err := findChecksum(string(list), filepath.Base(r.CloudImageURL))
	if err != nil {
		return fmt.Errorf("%s: %w", r.ChecksumURL, err)
	}
	got := sum256
	if len(want) == len(sum512) {
		got = sum512
	}
	if want != got {
		return fmt.Errorf("イメージのチェックサムが一致しない (期待 %s、実際 %s)", want, got)
	}
	return nil
}

func findChecksum(list, name string) (string, error) {
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			sum := strings.ToLower(fields[0])
			if _, err := hex.DecodeString(sum); err != nil || (len(sum) != 64 && len(sum) != 128) {
				return "", fmt.Errorf("%s のチェックサムが sha256 / sha512 の形でない", name)
			}
			return sum, nil
		}
	}
	return "", fmt.Errorf("%s の行が無い", name)
}

// verifySignature はイメージの分離署名をレシピの公開鍵だけで検証する (利用者の鍵束は使わない)。
func verifySignature(r Recipe, image string) error {
	sig, err := fetchSmall(r.SignatureURL, 64<<10)
	if err != nil {
		return err
	}
	return verifySignatureWith(r, sig, image)
}

func verifySignatureWith(r Recipe, sig []byte, image string) error {
	dir, err := os.MkdirTemp("", "quagent-gpg-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	keyring, sigFile := filepath.Join(dir, "key.gpg"), filepath.Join(dir, "image.sig")
	if err := os.WriteFile(sigFile, sig, 0o600); err != nil {
		return err
	}
	dearmor := exec.Command("gpg", "--homedir", dir, "--batch", "--dearmor", "-o", keyring)
	dearmor.Stdin = bytes.NewReader(r.signingKey)
	if out, err := dearmor.CombinedOutput(); err != nil {
		return fmt.Errorf("署名の公開鍵 %s を読めない (gpg が要る): %v: %s", r.SigningKey, err, out)
	}
	if out, err := exec.Command("gpgv", "--homedir", dir, "--keyring", keyring, sigFile, image).CombinedOutput(); err != nil {
		return fmt.Errorf("イメージの署名を検証できない: %v: %s", err, out)
	}
	return nil
}

func runWithTimeout(cmd *exec.Cmd, d time.Duration) ([]byte, error) {
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return []byte(buf.String()), err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		<-done
		return []byte(buf.String()), fmt.Errorf("%s でタイムアウト", d)
	}
}
