package sandbox

import (
	"encoding/json"
	"runtime"
)

// DockerSeccompProfile は Docker / OCI 互換の seccomp プロファイル仕様。
type DockerSeccompProfile struct {
	DefaultAction string              `json:"defaultAction"`
	Architectures []string            `json:"architectures"`
	Syscalls      []DockerSeccompRule `json:"syscalls"`
}

// DockerSeccompRule はシステムコールに対するアクションと条件ルール。
type DockerSeccompRule struct {
	Names  []string               `json:"names"`
	Action string                 `json:"action"`
	Args   []DockerSeccompArgRule `json:"args,omitempty"`
}

// DockerSeccompArgRule はシステムコールの引数に対する条件比較。
type DockerSeccompArgRule struct {
	Index uint   `json:"index"`
	Value uint64 `json:"value"`
	Op    string `json:"op"`
}

// dockerArchitectures は現在の CPU アーキテクチャに応じた Docker seccomp のアーキテクチャ識別子を返す。
func dockerArchitectures() []string {
	switch runtime.GOARCH {
	case "arm64":
		return []string{"SCMP_ARCH_AARCH64"}
	default: // amd64 / x86_64
		return []string{"SCMP_ARCH_X86_64"}
	}
}

// GenerateDockerSeccomp は Policy の設定内容に基づいた Docker 向け seccomp プロファイル JSON を生成する。
// コンテナ内からの AF_VSOCK ソケット作成や、Policy で指定された危険システムコールを遮断する。
func (p *Policy) GenerateDockerSeccomp() ([]byte, error) {
	prof := DockerSeccompProfile{
		DefaultAction: "SCMP_ACT_ALLOW",
		Architectures: dockerArchitectures(),
		Syscalls:      make([]DockerSeccompRule, 0),
	}

	// 1. AF_VSOCK ソケット作成の遮断 (socket(domain == AF_VSOCK(40), ...))
	if p.DenyVsockOn() {
		prof.Syscalls = append(prof.Syscalls, DockerSeccompRule{
			Names:  []string{"socket"},
			Action: "SCMP_ACT_ERRNO",
			Args: []DockerSeccompArgRule{
				{
					Index: 0,
					Value: 40, // unix.AF_VSOCK
					Op:    "SCMP_CMP_EQ",
				},
			},
		})
	}

	// 2. Policy で指定された危険システムコールの拒否 (compat / strict / extra_deny)
	denyList := p.DenyNames()
	if len(denyList) > 0 {
		prof.Syscalls = append(prof.Syscalls, DockerSeccompRule{
			Names:  denyList,
			Action: "SCMP_ACT_ERRNO",
		})
	}

	return json.MarshalIndent(prof, "", "  ")
}

// DockerSeccompJSON は GenerateDockerSeccomp のエイリアス。
func (p *Policy) DockerSeccompJSON() ([]byte, error) {
	return p.GenerateDockerSeccomp()
}

// DockerDaemonConfig は Docker デーモンの設定構造体。
type DockerDaemonConfig struct {
	SeccompProfile string `json:"seccomp-profile,omitempty"`
}

// GenerateDockerDaemonJSON は 指定された seccomp プロファイルパスを設定した daemon.json を生成する。
func GenerateDockerDaemonJSON(seccompProfilePath string) ([]byte, error) {
	cfg := DockerDaemonConfig{
		SeccompProfile: seccompProfilePath,
	}
	return json.MarshalIndent(cfg, "", "  ")
}
