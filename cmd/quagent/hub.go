package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/nananek/quagent/internal/config"
	"github.com/nananek/quagent/internal/hub"
	"github.com/nananek/quagent/internal/notify"
	"github.com/nananek/quagent/internal/paths"
)

// cmdHub は Web 遠隔操作モード (Tailscale 越しにブラウザからセッションを操作する) を
// 起動する。run 側で登録を有効にしたセッションが対象になる。
func cmdHub(args []string) error {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	listen := fs.String("listen", "", `待ち受けアドレス ("tailscale" または host:port。既定: config の hub.listen)`)
	port := fs.Int("port", 0, "待ち受けポート (既定: config の hub.port)")
	baseURL := fs.String("base-url", "", "外部から見た基点 URL (既定: config の hub.base_url)")
	_ = fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pol := cfg.Hub
	if !pol.IsEnabled() {
		return fmt.Errorf("hub は無効。%s の hub.enabled を true にしてから起動してください", config.Path())
	}

	addr, base, err := resolveHubListen(pol, *listen, *port, *baseURL)
	if err != nil {
		return err
	}

	webhook, err := pol.Notify.Webhook()
	if err != nil {
		return fmt.Errorf("Discord Webhook を取得できない: %w", err)
	}
	notifier := notify.New(webhook)
	defer notifier.Close()
	if notifier.Enabled() {
		logf("Discord Webhook 通知: 有効")
	}

	srv := hub.New(hub.Config{
		Addr:               addr,
		BaseURL:            base,
		Notifier:           notifier,
		NotifySessionStart: pol.Notify.NotifyOnSessionStart(),
		NotifyApproval:     pol.Notify.NotifyOnApproval(),
		Logger:             func(s string) { logf("%s", s) },
	})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		<-sig
		logf("hub を終了する")
		srv.Close()
	}()

	logf("hub を起動: %s (run 側は --hub で登録)", srv.BaseURL())
	return srv.Serve(paths.HubSocket())
}

// resolveHubListen は待ち受けアドレスと外部から見た基点 URL を決める。
func resolveHubListen(pol *config.HubPolicy, listenOverride string, portOverride int, baseOverride string) (addr, base string, err error) {
	listen := pol.ListenResolved()
	if listenOverride != "" {
		listen = listenOverride
	}
	port := pol.PortResolved()
	if portOverride > 0 {
		port = portOverride
	}

	if listen == "tailscale" {
		ip, err := tailscaleIP()
		if err != nil {
			return "", "", err
		}
		addr = net.JoinHostPort(ip, strconv.Itoa(port))
	} else {
		addr = listen
		if _, _, e := net.SplitHostPort(listen); e != nil {
			addr = net.JoinHostPort(listen, strconv.Itoa(port))
		}
	}

	base = pol.BaseURL
	if baseOverride != "" {
		base = baseOverride
	}
	if base == "" {
		base = "http://" + addr
	}
	return addr, strings.TrimRight(base, "/"), nil
}

// tailscaleIP は Tailscale の IPv4 アドレスを返す。
func tailscaleIP() (string, error) {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return "", fmt.Errorf("Tailscale の IP を取得できない (tailscale ip -4): %w", err)
	}
	ip := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("Tailscale の IP が不正: %q", ip)
	}
	return ip, nil
}
