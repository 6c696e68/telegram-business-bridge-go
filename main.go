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

const version = "1.8.0"

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
	llm := NewLLMChain(cfg.APIBase, cfg.APIKey, cfg.modelChain(), hc)
	llm.MaxTokens = cfg.MaxTokens
	llm.Temperature = cfg.Temperature
	b := NewBridge(cfg, NewTelegram(cfg.TelegramBase, cfg.BotToken, hc), llm)
	if b.dbErr != nil {
		logf("ERROR", "%s", b.dbErr.Error())
		os.Exit(2)
	}

	// Image turns go to a model that actually accepts images. Some relays answer
	// a multimodal request with 200 OK and silently drop the image parts, so the
	// main model can stay text-only while vision_model reads photos.
	if cfg.VisionModel != "" {
		vbase, vkey := cfg.VisionAPIBase, cfg.VisionAPIKey
		if vbase == "" {
			vbase = cfg.APIBase
		}
		if vkey == "" {
			vkey = cfg.APIKey
		}
		vllm := NewLLMChain(vbase, vkey, []string{cfg.VisionModel}, hc)
		vllm.MaxTokens = cfg.MaxTokens
		vllm.Temperature = cfg.Temperature
		b.visionLLM = vllm
		logf("INFO", "vision model: %s", cfg.VisionModel)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go b.statsLoop(ctx)
	presenceDone := make(chan struct{})
	go func() {
		b.presenceLoop(ctx)
		close(presenceDone)
	}()
	go b.startHealth(ctx, cfg.HealthAddr)

	flushDone := make(chan struct{})
	go func() {
		b.flushLoop(ctx)
		close(flushDone)
	}()

	runErr := b.Run(ctx)
	// Always cancel before joining the workers: on a fatal Run error the
	// signal context is still live and waiting on it would hang forever.
	stop()
	<-flushDone
	<-presenceDone // no follow-up job may start once draining begins
	if runErr != nil {
		logf("ERROR", "fatal: %v", runErr)
		_ = b.Close()
		os.Exit(1)
	}
	if !b.Drain(time.Duration(cfg.ShutdownDrainSec) * time.Second) {
		logf("WARN", "shutdown drain timed out; some replies may be cut")
	}
	if err := b.Close(); err != nil {
		logf("WARN", "final flush failed: %s", err)
	}
	logf("INFO", "stopped cleanly")
}
