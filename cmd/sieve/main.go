// Command sieve runs the Sieve LLM inference gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Prm01/sieve/internal/cache"
	"github.com/Prm01/sieve/internal/config"
	"github.com/Prm01/sieve/internal/embed"
	"github.com/Prm01/sieve/internal/privacy"
	"github.com/Prm01/sieve/internal/provider"
	"github.com/Prm01/sieve/internal/ratelimit"
	"github.com/Prm01/sieve/internal/resp"
	"github.com/Prm01/sieve/internal/router"
	"github.com/Prm01/sieve/internal/server"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	var handler slog.Handler = slog.NewJSONHandler(os.Stdout, nil)
	if strings.EqualFold(os.Getenv("SIEVE_LOG"), "text") {
		handler = slog.NewTextHandler(os.Stdout, nil)
	}
	log := slog.New(handler)
	if err := run(log); err != nil {
		log.Error("sieve stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var emb embed.Embedder
	switch cfg.Embedder.Type {
	case "openai":
		emb = &embed.OpenAI{BaseURL: cfg.Embedder.BaseURL, APIKey: cfg.Embedder.APIKey, Model: cfg.Embedder.Model}
	default:
		stop := cfg.Stopwords
		if stop == nil {
			stop = embed.DefaultStopwords
		}
		emb = embed.NewHash(cfg.Embedder.Dim, stop, cfg.Synonyms)
	}

	var rdb *resp.Client
	if cfg.RedisURL != "" {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		rdb, err = resp.Dial(dctx, cfg.RedisURL, 32)
		cancel()
		if err != nil {
			return fmt.Errorf("connect to redis: %w", err)
		}
		defer rdb.Close()
	}

	var store cache.Persister
	if rdb != nil {
		store = &cache.Redis{C: rdb, TTL: cfg.TTL.Duration}
	}
	c := cache.New(cache.Config{Threshold: float32(cfg.Threshold), TTL: cfg.TTL.Duration, MaxEntries: cfg.MaxEntries}, store, log)
	go c.Sync(ctx, cfg.SyncInterval.Duration)

	rl := ratelimit.Config{Capacity: cfg.RateLimit.Capacity, RefillPerSec: cfg.RateLimit.RefillPerSec}
	var limiter ratelimit.Limiter = ratelimit.NewMemory(rl)
	if rdb != nil {
		limiter = ratelimit.FailOpen{L: &ratelimit.Redis{C: rdb, Cfg: rl}}
	}

	var provs []provider.Provider
	for _, p := range cfg.Providers {
		switch p.Type {
		case "openai":
			provs = append(provs, &provider.OpenAI{ID: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model})
		case "anthropic":
			provs = append(provs, &provider.Anthropic{ID: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model})
		case "gemini":
			provs = append(provs, &provider.Gemini{ID: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model})
		case "mock":
			provs = append(provs, &provider.Mock{ID: p.Name, Latency: time.Duration(p.LatencyMS) * time.Millisecond, Jitter: time.Duration(p.JitterMS) * time.Millisecond, FailRate: p.FailRate})
		}
	}

	det, err := privacy.New(cfg.PrivacyPatterns)
	if err != nil {
		return err
	}
	m := server.NewMetrics(c)
	rt := router.New(router.Config{Timeout: cfg.Router.Timeout.Duration, FailureThreshold: cfg.Router.FailureThreshold, Cooldown: cfg.Router.Cooldown.Duration}, provs, m.RouterObserver())
	srv := server.New(server.Options{
		Cache: c, Embedder: emb, Router: rt, Limiter: limiter, Privacy: det, Metrics: m, Log: log,
		APIKeys: cfg.APIKeys, AdminKey: cfg.AdminKey, MaxPromptLen: cfg.MaxPromptLen, CORSOrigins: cfg.CORSOrigins, Version: version,
	})

	names := make([]string, len(provs))
	for i, p := range provs {
		names[i] = p.Name()
	}
	hs := &http.Server{Addr: cfg.Addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("sieve listening", "addr", cfg.Addr, "version", version, "providers", names, "embedder", emb.Name(),
		"threshold", cfg.Threshold, "redis", rdb != nil, "auth", len(cfg.APIKeys) > 0)
	if len(cfg.APIKeys) == 0 {
		log.Warn("no API keys configured: the gateway is open to anyone who can reach it")
	}

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return hs.Shutdown(sctx)
	}
	return nil
}
