package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	LogLevel      string        `json:"log_level"`
	UDPIdleString string        `json:"udp_idle_timeout"`
	Routes        []RouteConfig `json:"routes"`
}

type RouteConfig struct {
	Name   string `json:"name"`
	Proto  string `json:"proto"`
	Listen string `json:"listen"`
	Target string `json:"target"`
}

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	errCh := make(chan error, len(cfg.Routes))
	for _, route := range cfg.Routes {
		route := route
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runRoute(ctx, cfg, route); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("%s: %w", routeLabel(route), err)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(errCh)
	}()

	for err := range errCh {
		log.Print(err)
		stop()
	}
}

func loadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()

	var cfg Config
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return Config{}, err
	}
	if len(cfg.Routes) == 0 {
		return Config{}, errors.New("at least one route is required")
	}
	for i, route := range cfg.Routes {
		if route.Proto != "tcp" && route.Proto != "udp" {
			return Config{}, fmt.Errorf("routes[%d].proto must be tcp or udp", i)
		}
		if route.Listen == "" {
			return Config{}, fmt.Errorf("routes[%d].listen is required", i)
		}
		if route.Target == "" {
			return Config{}, fmt.Errorf("routes[%d].target is required", i)
		}
	}
	return cfg, nil
}

func runRoute(ctx context.Context, cfg Config, route RouteConfig) error {
	switch route.Proto {
	case "tcp":
		return runTCP(ctx, route)
	case "udp":
		idle := 2 * time.Minute
		if cfg.UDPIdleString != "" {
			parsed, err := time.ParseDuration(cfg.UDPIdleString)
			if err != nil {
				return fmt.Errorf("invalid udp_idle_timeout: %w", err)
			}
			idle = parsed
		}
		return runUDP(ctx, route, idle)
	default:
		return fmt.Errorf("unsupported proto %q", route.Proto)
	}
}

func runTCP(ctx context.Context, route RouteConfig) error {
	listener, err := net.Listen("tcp", route.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()

	log.Printf("tcp route %s listening on %s -> %s", routeLabel(route), route.Listen, route.Target)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return context.Canceled
			}
			log.Printf("tcp accept failed on %s: %v", routeLabel(route), err)
			continue
		}
		go handleTCP(ctx, route, conn)
	}
}

func handleTCP(ctx context.Context, route RouteConfig, client net.Conn) {
	defer client.Close()

	target, err := net.Dial("tcp", route.Target)
	if err != nil {
		log.Printf("tcp dial failed %s -> %s: %v", client.RemoteAddr(), route.Target, err)
		return
	}
	defer target.Close()

	log.Printf("tcp open %s %s -> %s", routeLabel(route), client.RemoteAddr(), route.Target)
	defer log.Printf("tcp close %s %s -> %s", routeLabel(route), client.RemoteAddr(), route.Target)

	done := make(chan struct{}, 2)
	go copyAndClose(target, client, done)
	go copyAndClose(client, target, done)

	select {
	case <-ctx.Done():
	case <-done:
	}
}

func copyAndClose(dst net.Conn, src net.Conn, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	if tcp, ok := dst.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	} else {
		_ = dst.Close()
	}
	done <- struct{}{}
}

func runUDP(ctx context.Context, route RouteConfig, idle time.Duration) error {
	listenAddr, err := net.ResolveUDPAddr("udp", route.Listen)
	if err != nil {
		return err
	}
	targetAddr, err := net.ResolveUDPAddr("udp", route.Target)
	if err != nil {
		return err
	}
	listener, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()

	log.Printf("udp route %s listening on %s -> %s", routeLabel(route), route.Listen, route.Target)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	sessions := newUDPSessions(listener, targetAddr, idle)
	defer sessions.closeAll()

	buffer := make([]byte, 64*1024)
	for {
		n, clientAddr, err := listener.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return context.Canceled
			}
			log.Printf("udp read failed on %s: %v", routeLabel(route), err)
			continue
		}
		packet := append([]byte(nil), buffer[:n]...)
		session, err := sessions.get(ctx, route, clientAddr)
		if err != nil {
			log.Printf("udp session failed %s -> %s: %v", clientAddr, route.Target, err)
			continue
		}
		session.touch()
		if _, err := session.upstream.Write(packet); err != nil {
			log.Printf("udp write failed %s -> %s: %v", clientAddr, route.Target, err)
		}
	}
}

type udpSessions struct {
	listener *net.UDPConn
	target   *net.UDPAddr
	idle     time.Duration
	mu       sync.Mutex
	items    map[string]*udpSession
}

type udpSession struct {
	client   *net.UDPAddr
	upstream *net.UDPConn
	mu       sync.Mutex
	lastSeen time.Time
	close    context.CancelFunc
}

func newUDPSessions(listener *net.UDPConn, target *net.UDPAddr, idle time.Duration) *udpSessions {
	return &udpSessions{
		listener: listener,
		target:   target,
		idle:     idle,
		items:    make(map[string]*udpSession),
	}
}

func (s *udpSessions) get(ctx context.Context, route RouteConfig, client *net.UDPAddr) (*udpSession, error) {
	key := client.String()
	s.mu.Lock()
	if session, ok := s.items[key]; ok {
		s.mu.Unlock()
		return session, nil
	}
	s.mu.Unlock()

	upstream, err := net.DialUDP("udp", nil, s.target)
	if err != nil {
		return nil, err
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	session := &udpSession{
		client:   client,
		upstream: upstream,
		lastSeen: time.Now(),
		close:    cancel,
	}

	s.mu.Lock()
	if existing, ok := s.items[key]; ok {
		s.mu.Unlock()
		cancel()
		_ = upstream.Close()
		return existing, nil
	}
	s.items[key] = session
	s.mu.Unlock()

	log.Printf("udp open %s %s -> %s", routeLabel(route), client, route.Target)
	go s.relayReplies(sessionCtx, route, key, session)
	go s.expireIdle(sessionCtx, route, key, session)
	return session, nil
}

func (s *udpSessions) relayReplies(ctx context.Context, route RouteConfig, key string, session *udpSession) {
	buffer := make([]byte, 64*1024)
	for {
		n, err := session.upstream.Read(buffer)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("udp upstream read closed %s %s: %v", routeLabel(route), session.client, err)
			}
			s.delete(key, session)
			return
		}
		session.touch()
		if _, err := s.listener.WriteToUDP(buffer[:n], session.client); err != nil {
			log.Printf("udp reply write failed %s -> %s: %v", route.Target, session.client, err)
		}
	}
}

func (s *udpSessions) expireIdle(ctx context.Context, route RouteConfig, key string, session *udpSession) {
	ticker := time.NewTicker(minDuration(s.idle/2, 30*time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.delete(key, session)
			return
		case <-ticker.C:
			if time.Since(session.seen()) > s.idle {
				log.Printf("udp idle close %s %s -> %s", routeLabel(route), session.client, route.Target)
				s.delete(key, session)
				return
			}
		}
	}
}

func (s *udpSessions) delete(key string, session *udpSession) {
	s.mu.Lock()
	if current, ok := s.items[key]; ok && current == session {
		delete(s.items, key)
	}
	s.mu.Unlock()
	session.close()
	_ = session.upstream.Close()
}

func (s *udpSessions) closeAll() {
	s.mu.Lock()
	items := make([]*udpSession, 0, len(s.items))
	for _, session := range s.items {
		items = append(items, session)
	}
	s.items = make(map[string]*udpSession)
	s.mu.Unlock()
	for _, session := range items {
		session.close()
		_ = session.upstream.Close()
	}
}

func (s *udpSession) touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeen = time.Now()
}

func (s *udpSession) seen() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeen
}

func routeLabel(route RouteConfig) string {
	if route.Name != "" {
		return route.Name
	}
	return route.Proto + ":" + route.Listen
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}
