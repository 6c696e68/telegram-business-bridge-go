package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
)

const version = "1.3.0"

func newHTTPClient(maxIdle int) *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxIdle,
		MaxIdleConnsPerHost:   maxIdle,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0, // per-request deadlines come from context
	}
	return &http.Client{Transport: tr}
}

func main() {
	defaultCfg := os.Getenv("BRIDGE_CONFIG")
	if defaultCfg == "" {
		defaultCfg = "/root/tg-business-bridge/config.json"
	}
	cfgPath := flag.String("config", defaultCfg, "path to config.json")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		logger.Println("tg-bridge", version)
		return
	}

	// A small heap target keeps RSS low on a 1 GB router without slowing the
	// I/O-bound workload.
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(64 << 20)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		logf("ERROR", "config: %v", err)
		os.Exit(2)
	}

	hc := newHTTPClient(4)
	b := NewBridge(cfg, NewTelegram(cfg.TelegramBase, cfg.BotToken, hc),
		NewLLMChain(cfg.APIBase, cfg.APIKey, cfg.modelChain(), hc))
	if b.dbErr != nil {
		logf("ERROR", "%s", b.dbErr.Error())
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go b.statsLoop(ctx)
	go b.startHealth(ctx, cfg.HealthAddr)

	flushDone := make(chan struct{})
	go func() {
		b.flushLoop(ctx)
		close(flushDone)
	}()

	if err := b.Run(ctx); err != nil {
		logf("ERROR", "fatal: %v", err)
		<-flushDone
		_ = b.Close()
		os.Exit(1)
	}
	<-flushDone
	if !b.Drain(time.Duration(cfg.ShutdownDrainSec) * time.Second) {
		logf("WARN", "shutdown drain timed out; some replies may be cut")
	}
	if err := b.Close(); err != nil {
		logf("WARN", "final flush failed: %s", err)
	}
	logf("INFO", "stopped cleanly")
}
