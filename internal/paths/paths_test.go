package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathsWithEnv(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))

	if got := DataDir(); got != filepath.Join(tmp, "data", "quagent") {
		t.Errorf("DataDir() = %q, want %q", got, filepath.Join(tmp, "data", "quagent"))
	}
	if got := CacheDir(); got != filepath.Join(tmp, "cache", "quagent") {
		t.Errorf("CacheDir() = %q, want %q", got, filepath.Join(tmp, "cache", "quagent"))
	}
	if got := StateDir(); got != filepath.Join(tmp, "state", "quagent") {
		t.Errorf("StateDir() = %q, want %q", got, filepath.Join(tmp, "state", "quagent"))
	}
	if got := ImagesDir(); got != filepath.Join(tmp, "data", "quagent", "images") {
		t.Errorf("ImagesDir() = %q, want %q", got, filepath.Join(tmp, "data", "quagent", "images"))
	}
	if got := RunsDir(); got != filepath.Join(tmp, "state", "quagent", "runs") {
		t.Errorf("RunsDir() = %q, want %q", got, filepath.Join(tmp, "state", "quagent", "runs"))
	}
	if got := LogsDir(); got != filepath.Join(tmp, "state", "quagent", "logs") {
		t.Errorf("LogsDir() = %q, want %q", got, filepath.Join(tmp, "state", "quagent", "logs"))
	}
	if got := ConfigFile(); got != filepath.Join(tmp, "config", "quagent", "config.json") {
		t.Errorf("ConfigFile() = %q, want %q", got, filepath.Join(tmp, "config", "quagent", "config.json"))
	}
	if got := SkillsDir(); got != filepath.Join(tmp, "config", "quagent", "skills") {
		t.Errorf("SkillsDir() = %q, want %q", got, filepath.Join(tmp, "config", "quagent", "skills"))
	}
	if got := DockerCacheDir(); got != filepath.Join(tmp, "cache", "quagent", "docker") {
		t.Errorf("DockerCacheDir() = %q, want %q", got, filepath.Join(tmp, "cache", "quagent", "docker"))
	}
}

func TestPathsFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	if got := DataDir(); got != filepath.Join(home, ".local/share", "quagent") {
		t.Errorf("DataDir() = %q, want %q", got, filepath.Join(home, ".local/share", "quagent"))
	}
	if got := CacheDir(); got != filepath.Join(home, ".cache", "quagent") {
		t.Errorf("CacheDir() = %q, want %q", got, filepath.Join(home, ".cache", "quagent"))
	}
	if got := StateDir(); got != filepath.Join(home, ".local/state", "quagent") {
		t.Errorf("StateDir() = %q, want %q", got, filepath.Join(home, ".local/state", "quagent"))
	}
	if got := ConfigFile(); got != filepath.Join(home, ".config", "quagent", "config.json") {
		t.Errorf("ConfigFile() = %q, want %q", got, filepath.Join(home, ".config", "quagent", "config.json"))
	}
	if got := SkillsDir(); got != filepath.Join(home, ".config", "quagent", "skills") {
		t.Errorf("SkillsDir() = %q, want %q", got, filepath.Join(home, ".config", "quagent", "skills"))
	}
	if got := DockerCacheDir(); got != filepath.Join(home, ".cache", "quagent", "docker") {
		t.Errorf("DockerCacheDir() = %q, want %q", got, filepath.Join(home, ".cache", "quagent", "docker"))
	}
}
