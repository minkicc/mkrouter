package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/minkicc/mkrouter/backend/internal/config"
	"github.com/minkicc/mkrouter/backend/internal/engine"
	"github.com/minkicc/mkrouter/backend/internal/server"
)

//go:embed web/*
var webFS embed.FS

func main() {
	cfgPath := flag.String("config", firstEnv("MKROUTER_CONFIG_PATH", "MKSWITCH_CONFIG_PATH"), "path to config.json")
	noBrowser := flag.Bool("no-browser", envEnabled("MKROUTER_NO_BROWSER", "MKSWITCH_NO_BROWSER"), "do not open the admin page in a browser")
	listenAddr := flag.String("listen", firstEnv("MKROUTER_LISTEN_ADDR", "MKSWITCH_LISTEN_ADDR"), "override listen address")
	flag.Parse()

	if strings.TrimSpace(*cfgPath) == "" {
		*cfgPath = config.DefaultPath()
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if strings.TrimSpace(*listenAddr) != "" {
		cfg.ListenAddr = strings.TrimSpace(*listenAddr)
	}
	cfg.Normalize()
	if err := cfg.Save(*cfgPath); err != nil {
		log.Fatalf("save config: %v", err)
	}

	eng, err := engine.New(cfg)
	if err != nil {
		log.Fatalf("init engine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)

	uiFS, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed web ui: %v", err)
	}
	srv := server.New(eng, *cfgPath, uiFS, log.Default())
	srv.Start(ctx)
	handler := srv.Handler()

	bindAddr := cfg.ListenAddr
	if cfg.AllowLAN {
		bindAddr = lanListenAddr(cfg.ListenAddr)
	}
	httpServer := &http.Server{
		Addr:              bindAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("mkrouter listening on http://%s", bindAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	if !*noBrowser {
		time.Sleep(300 * time.Millisecond)
		_ = openBrowser("http://127.0.0.1:" + portOf(cfg.ListenAddr))
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func envEnabled(names ...string) bool {
	for _, name := range names {
		if os.Getenv(name) == "1" {
			return true
		}
	}
	return false
}

func displayAddr(addr string) string {
	host := strings.TrimSpace(addr)
	if strings.HasPrefix(host, "0.0.0.0") || strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + strings.TrimPrefix(host, "0.0.0.0")
	} else if !strings.Contains(host, ":") {
		host = host + ":8787"
	}
	return host
}

func lanListenAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "0.0.0.0:8787"
	}
	if strings.HasPrefix(addr, ":") {
		return "0.0.0.0" + addr
	}
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return "0.0.0.0" + addr[idx:]
	}
	return "0.0.0.0:" + addr
}

func portOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		port := addr[idx+1:]
		if port != "" {
			return port
		}
	}
	return "8787"
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if cmd == nil {
		return fmt.Errorf("unsupported os %s", runtime.GOOS)
	}
	return cmd.Start()
}
