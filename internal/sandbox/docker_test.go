package sandbox

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestGenerateDockerSeccomp_Default(t *testing.T) {
	p := Default()
	b, err := p.GenerateDockerSeccomp()
	if err != nil {
		t.Fatalf("GenerateDockerSeccomp failed: %v", err)
	}

	var prof DockerSeccompProfile
	if err := json.Unmarshal(b, &prof); err != nil {
		t.Fatalf("failed to unmarshal generated seccomp profile: %v", err)
	}

	if prof.DefaultAction != "SCMP_ACT_ALLOW" {
		t.Errorf("expected DefaultAction SCMP_ACT_ALLOW, got %s", prof.DefaultAction)
	}
	if len(prof.Architectures) == 0 {
		t.Errorf("expected non-empty Architectures")
	}

	// Verify AF_VSOCK socket rule
	foundSocket := false
	for _, rule := range prof.Syscalls {
		if slices.Contains(rule.Names, "socket") && len(rule.Args) == 1 {
			if rule.Action != "SCMP_ACT_ERRNO" {
				t.Errorf("expected socket rule action SCMP_ACT_ERRNO, got %s", rule.Action)
			}
			if rule.Args[0].Index == 0 && rule.Args[0].Value == 40 && rule.Args[0].Op == "SCMP_CMP_EQ" {
				foundSocket = true
			}
		}
	}
	if !foundSocket {
		t.Errorf("expected socket rule with AF_VSOCK (40) check, none found")
	}

	// Verify compat denied syscalls
	foundCompat := false
	for _, rule := range prof.Syscalls {
		if slices.Contains(rule.Names, "bpf") && slices.Contains(rule.Names, "ptrace") {
			foundCompat = true
			if rule.Action != "SCMP_ACT_ERRNO" {
				t.Errorf("expected compat rule action SCMP_ACT_ERRNO, got %s", rule.Action)
			}
		}
	}
	if !foundCompat {
		t.Errorf("expected compat denied syscalls, none found")
	}
}

func TestGenerateDockerSeccomp_StrictAndExtraDeny(t *testing.T) {
	denyVsock := false
	p := &Policy{
		Mode:      "strict",
		DenyVsock: &denyVsock,
		ExtraDeny: []string{"chroot", "custom_sys"},
	}

	b, err := p.DockerSeccompJSON()
	if err != nil {
		t.Fatalf("DockerSeccompJSON failed: %v", err)
	}

	var prof DockerSeccompProfile
	if err := json.Unmarshal(b, &prof); err != nil {
		t.Fatalf("failed to unmarshal generated seccomp profile: %v", err)
	}

	// DenyVsock is false -> no socket rule
	for _, rule := range prof.Syscalls {
		if slices.Contains(rule.Names, "socket") {
			t.Errorf("did not expect socket rule when DenyVsock is false")
		}
	}

	// Verify strict syscalls (e.g. mount, unshare) and ExtraDeny
	foundMount := false
	foundCustom := false
	for _, rule := range prof.Syscalls {
		if slices.Contains(rule.Names, "mount") {
			foundMount = true
		}
		if slices.Contains(rule.Names, "custom_sys") {
			foundCustom = true
		}
	}
	if !foundMount {
		t.Errorf("expected strict syscall 'mount' to be denied")
	}
	if !foundCustom {
		t.Errorf("expected extra syscall 'custom_sys' to be denied")
	}
}

func TestGenerateDockerDaemonJSON(t *testing.T) {
	b, err := GenerateDockerDaemonJSON("/etc/quagent/docker-seccomp.json")
	if err != nil {
		t.Fatalf("GenerateDockerDaemonJSON failed: %v", err)
	}

	var cfg DockerDaemonConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("failed to unmarshal daemon config: %v", err)
	}
	if cfg.SeccompProfile != "/etc/quagent/docker-seccomp.json" {
		t.Errorf("unexpected seccomp-profile: %s", cfg.SeccompProfile)
	}

	// Test with registry mirrors
	bWithMirrors, err := GenerateDockerDaemonJSON("/etc/quagent/docker-seccomp.json", "http://127.0.0.1:5000")
	if err != nil {
		t.Fatalf("GenerateDockerDaemonJSON with mirrors failed: %v", err)
	}
	var cfgWithMirrors DockerDaemonConfig
	if err := json.Unmarshal(bWithMirrors, &cfgWithMirrors); err != nil {
		t.Fatalf("failed to unmarshal daemon config: %v", err)
	}
	if len(cfgWithMirrors.RegistryMirrors) != 1 || cfgWithMirrors.RegistryMirrors[0] != "http://127.0.0.1:5000" {
		t.Errorf("unexpected registry-mirrors: %v", cfgWithMirrors.RegistryMirrors)
	}
}

