package netns

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"runtime"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

// listenInNetns は子 netns の中に DNS 用の UDP/TCP socket を作る。
//
// socket はスレッドの netns に作られる。userns の root は子 netns へは setns
// できるが host の netns へは戻れないので、専用スレッドで入って作り、
// そのスレッドは捨てる (UnlockOSThread しないまま goroutine を終えると破棄される)。
func listenInNetns(holderPid int, addr string) (net.PacketConn, net.Listener, error) {
	type result struct {
		pc  net.PacketConn
		l   net.Listener
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		fd, err := unix.Open(fmt.Sprintf("/proc/%d/ns/net", holderPid), unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer unix.Close(fd)
		if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
			ch <- result{err: fmt.Errorf("setns: %w", err)}
			return
		}
		pc, err := net.ListenPacket("udp4", addr)
		if err != nil {
			ch <- result{err: err}
			return
		}
		l, err := net.Listen("tcp4", addr)
		if err != nil {
			pc.Close()
			ch <- result{err: err}
			return
		}
		ch <- result{pc: pc, l: l}
	}()
	r := <-ch
	return r.pc, r.l, r.err
}

// dnsServer は許可されたドメインの問い合わせだけを上流へ転送する。
// 応答の A レコードの IP を onAnswer で通知し、許可外は REFUSED を返す。
type dnsServer struct {
	// sem は同時に処理する問い合わせの上限 (UDP と TCP の合計)。溢れた分は捨てる。
	sem      chan struct{}
	upstream string
	allowed  func(name string) bool
	onAnswer func(name string, ips []net.IP, cnames []string)
	onDenied func(name string)
}

func (s *dnsServer) serveUDP(pc net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		select {
		case s.sem <- struct{}{}:
		default:
			continue // 溢れた問い合わせは捨てる (guest は再送する)
		}
		go func() {
			defer func() { <-s.sem }()
			if resp := s.handle(q, "udp"); resp != nil {
				_, _ = pc.WriteTo(resp, from)
			}
		}()
	}
}

func (s *dnsServer) serveTCP(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		select {
		case s.sem <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-s.sem }()
			defer c.Close()
			for {
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				var hdr [2]byte
				if _, err := io.ReadFull(c, hdr[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(hdr[:]))
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				resp := s.handle(q, "tcp")
				if resp == nil {
					return
				}
				out := binary.BigEndian.AppendUint16(nil, uint16(len(resp)))
				if _, err := c.Write(append(out, resp...)); err != nil {
					return
				}
			}
		}()
	}
}

func (s *dnsServer) handle(q []byte, proto string) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(q)
	if err != nil {
		return nil
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 {
		return reply(hdr, qs, dnsmessage.RCodeFormatError)
	}
	name := normalize(qs[0].Name.String())
	if !s.allowed(name) {
		s.onDenied(name)
		return reply(hdr, qs, dnsmessage.RCodeRefused)
	}
	// 子 netns は IPv4 しか持たないので AAAA は空で返し、v4 を使わせる
	if qs[0].Type == dnsmessage.TypeAAAA {
		return reply(hdr, qs, dnsmessage.RCodeSuccess)
	}
	resp, err := forward(s.upstream, q, proto)
	if err != nil {
		log.Printf("dns: 上流への転送に失敗 %s: %v", name, err)
		return reply(hdr, qs, dnsmessage.RCodeServerFailure)
	}
	if ips, cnames := answers(resp); len(ips) > 0 || len(cnames) > 0 {
		s.onAnswer(name, ips, cnames)
	}
	return resp
}

// forward は host の網から上流 DNS へ問い合わせる (このプロセスは host の netns にいる)。
func forward(upstream string, q []byte, proto string) ([]byte, error) {
	c, err := net.DialTimeout(proto, net.JoinHostPort(upstream, "53"), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if proto == "udp" {
		if _, err := c.Write(q); err != nil {
			return nil, err
		}
		buf := make([]byte, 65535)
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
	if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...)); err != nil {
		return nil, err
	}
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil, err
	}
	resp := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	_, err = io.ReadFull(c, resp)
	return resp, err
}

// answers は応答の A レコードの IP と CNAME の別名を返す。
func answers(msg []byte) (ips []net.IP, cnames []string) {
	var p dnsmessage.Parser
	if _, err := p.Start(msg); err != nil {
		return nil, nil
	}
	if err := p.SkipAllQuestions(); err != nil {
		return nil, nil
	}
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			break
		}
		switch h.Type {
		case dnsmessage.TypeA:
			a, err := p.AResource()
			if err != nil {
				return ips, cnames
			}
			ips = append(ips, net.IP(a.A[:]))
		case dnsmessage.TypeCNAME:
			c, err := p.CNAMEResource()
			if err != nil {
				return ips, cnames
			}
			cnames = append(cnames, normalize(c.CNAME.String()))
		default:
			if err := p.SkipAnswer(); err != nil {
				return ips, cnames
			}
		}
	}
	return ips, cnames
}

func reply(hdr dnsmessage.Header, qs []dnsmessage.Question, rcode dnsmessage.RCode) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: hdr.ID, Response: true, RecursionDesired: hdr.RecursionDesired,
		RecursionAvailable: true, RCode: rcode,
	})
	_ = b.StartQuestions()
	for _, q := range qs {
		_ = b.Question(q)
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

func normalize(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// Matches は許可パターンが name に当たるかを返す。"*.example.com" は
// サブドメインのみ、それ以外は完全一致。
func Matches(pattern, name string) bool {
	pattern = normalize(pattern)
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(name, "."+suffix)
	}
	return name == pattern
}
