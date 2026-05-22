package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"udp_idle_timeout": "30s",
		"routes": [
			{
				"name": "web",
				"proto": "tcp",
				"listen": "0.0.0.0:80",
				"target": "172.31.255.2:80"
			},
			{
				"name": "dns",
				"proto": "udp",
				"listen": "0.0.0.0:53",
				"target": "172.31.255.2:53"
			}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("got %d routes", len(cfg.Routes))
	}
}

func TestLoadConfigRejectsInvalidRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"routes": [
			{
				"proto": "icmp",
				"listen": "0.0.0.0:80",
				"target": "172.31.255.2:80"
			}
		]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected invalid proto error")
	}
}

func TestTCPRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping proxy integration test in short mode")
	}

	target := startTCPEcho(t)
	listener := listenTCP(t)
	listenAddr := listener.Addr().String()
	listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runTCP(ctx, RouteConfig{
			Name:   "test-tcp",
			Proto:  "tcp",
			Listen: listenAddr,
			Target: target,
		})
	}()
	waitForTCP(t, listenAddr)

	conn, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("got %q", string(buf))
	}

	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("runTCP returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runTCP did not stop")
	}
}

func TestUDPRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping proxy integration test in short mode")
	}

	target := startUDPEcho(t)
	listener := listenUDP(t)
	listenAddr := listener.LocalAddr().String()
	listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runUDP(ctx, RouteConfig{
			Name:   "test-udp",
			Proto:  "udp",
			Listen: listenAddr,
			Target: target,
		}, time.Second)
	}()
	waitForUDP(t, listenAddr)

	conn, err := net.Dial("udp", listenAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("got %q", string(buf))
	}

	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("runUDP returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runUDP did not stop")
	}
}

func startTCPEcho(t *testing.T) string {
	t.Helper()
	listener := listenTCP(t)
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 1024)
				n, err := conn.Read(buf)
				if err == nil {
					_, _ = conn.Write(buf[:n])
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func startUDPEcho(t *testing.T) string {
	t.Helper()
	conn := listenUDP(t)
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1024)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(buf[:n], addr)
		}
	}()
	return conn.LocalAddr().String()
}

func listenTCP(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func listenUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func waitForTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("tcp listener did not start on %s", addr)
}

func waitForUDP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("udp", addr)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("udp listener did not start on %s", addr)
}
