package pr

import (
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// originURL は repo の origin の URL を返す。
func originURL(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("origin の URL が空")
	}
	return s, nil
}

// RemoteHost は repo の origin のホスト名を小文字で返す。ポートは含めない。
// forge を origin のホストで選ぶために使う。
func RemoteHost(dir string) (string, error) {
	raw, err := originURL(dir)
	if err != nil {
		return "", fmt.Errorf("origin のホストを読めない: %w", err)
	}
	h, err := remoteHost(raw)
	if err != nil {
		return "", fmt.Errorf("origin のホストを読めない: %w", err)
	}
	return h, nil
}

// remoteHost はリモートの URL からホスト名を取り出す。
// https://host/... と ssh://host:port/... に加え、scp 形式 (git@host:owner/repo.git)
// も扱う。ポートと IPv6 のブラケットは外す。
func remoteHost(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("リモートの URL が空")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("リモートの URL を解釈できない: %w", err)
		}
		if u.Hostname() == "" {
			return "", fmt.Errorf("リモートにホストが無い: %q", raw)
		}
		return strings.ToLower(u.Hostname()), nil
	}
	// scp 形式。@ の後ろの、ブラケットの外にある最初の ":" までがホスト。
	if i := strings.Index(raw, "@"); i >= 0 {
		raw = raw[i+1:]
	}
	var host string
	if strings.HasPrefix(raw, "[") {
		end := strings.Index(raw, "]")
		if end < 0 || !strings.HasPrefix(raw[end+1:], ":") {
			return "", fmt.Errorf("対応していないリモートの形式: %q", raw)
		}
		host = raw[1:end]
	} else {
		i := strings.Index(raw, ":")
		if i <= 0 {
			return "", fmt.Errorf("対応していないリモートの形式: %q", raw)
		}
		host = raw[:i]
	}
	if host == "" {
		return "", fmt.Errorf("リモートにホストが無い: %q", raw)
	}
	return strings.ToLower(host), nil
}
