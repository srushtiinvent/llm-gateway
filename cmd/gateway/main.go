// Command gateway starts the LLM gateway server.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/auth"
	"github.com/srushtiinvent/llm-gateway/internal/cache"
	"github.com/srushtiinvent/llm-gateway/internal/config"
	"github.com/srushtiinvent/llm-gateway/internal/limiter"
	"github.com/srushtiinvent/llm-gateway/internal/provider"
	"github.com/srushtiinvent/llm-gateway/internal/quota"
	"github.com/srushtiinvent/llm-gateway/internal/router"
	"github.com/srushtiinvent/llm-gateway/internal/server"
	"github.com/srushtiinvent/llm-gateway/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\nCould not start the gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	config.LoadDotEnv(".env") // optional file with your settings

	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- storage: Postgres if DATABASE_URL is set, otherwise memory ---
	var st store.Store
	if cfg.DatabaseURL != "" {
		pg, err := store.NewPostgres(rootCtx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("cannot connect to the database (check DATABASE_URL): %w", err)
		}
		st = pg
	} else {
		st = store.NewMemory()
		logger.Warn("DATABASE_URL is not set: keys and logs are kept in memory and will be lost on restart")
	}
	defer st.Close()

	// --- admin token ---
	tempAdmin := false
	if cfg.AdminToken == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		cfg.AdminToken = "admin_" + hex.EncodeToString(b)
		tempAdmin = true
	}

	// --- providers ---
	var entries []*router.Entry
	for _, pc := range cfg.Providers {
		var p provider.Provider
		if pc.Mock {
			p = provider.NewMock(pc.Name, pc.Model, pc.MockLatency)
		} else {
			var headers map[string]string
			if pc.Name == "openrouter" {
				headers = map[string]string{"X-Title": "LLM Gateway"}
			}
			p = provider.NewOpenAICompat(pc.Name, pc.BaseURL, pc.APIKey, pc.Model, headers)
		}
		entries = append(entries, router.NewEntry(router.EntryConfig{
			Provider:         p,
			Limits:           quota.Limits{RPM: pc.RPM, RPD: pc.RPD, TPM: pc.TPM},
			MaxInputTokens:   pc.MaxInputTokens,
			Mock:             pc.Mock,
			BreakerThreshold: cfg.BreakerThreshold,
			BreakerOpenFor:   cfg.BreakerOpenFor,
		}))
	}
	opts := router.DefaultOptions()
	opts.MaxRetries = cfg.MaxRetries
	opts.AttemptTimeout = cfg.AttemptTimeout

	srv := server.New(server.Deps{
		Config:  cfg,
		Store:   st,
		Router:  router.New(entries, opts),
		Cache:   cache.New(cfg.CacheMaxEntries, cfg.CacheTTL),
		Queue:   limiter.NewQueue(cfg.MaxConcurrent, cfg.QueueMaxWaiting, cfg.QueueWaitTimeout),
		Limiter: limiter.New(),
		Auth:    auth.New(st, cfg.AdminToken),
		Budget:  auth.NewBudget(st),
		Logger:  logger,
	})

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: streaming answers can legitimately take a while.
	}

	// --- housekeeping: delete old logs once an hour ---
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-t.C:
				before := time.Now().AddDate(0, 0, -cfg.LogRetentionDays)
				if n, err := st.PurgeLogs(rootCtx, before); err != nil {
					logger.Warn("could not delete old logs", "err", err)
				} else if n > 0 {
					logger.Info("deleted old logs", "count", n)
				}
			}
		}
	}()

	banner(cfg, st, tempAdmin)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-rootCtx.Done():
		logger.Info("shutting down: finishing requests in progress...")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			logger.Warn("shutdown timed out", "err", err)
		}
	}
	srv.Close() // save the last request logs
	logger.Info("bye")
	return nil
}

func banner(cfg *config.Config, st store.Store, tempAdmin bool) {
	fmt.Println()
	fmt.Println("  LLM Gateway is running")
	fmt.Println("  ----------------------")
	fmt.Printf("  Address   : http://localhost:%s\n", cfg.Port)
	fmt.Printf("  Storage   : %s\n", st.Kind())
	fmt.Println("  Providers (in priority order):")
	for _, p := range cfg.Providers {
		kind := p.Model
		if p.Mock {
			kind = "FAKE provider, no API key needed"
		}
		fmt.Printf("    - %-16s %s  (limits: %d/min, %d/day)\n", p.Name, kind, p.RPM, p.RPD)
	}
	if tempAdmin {
		fmt.Println()
		fmt.Println("  ADMIN_TOKEN is not set, so a temporary one was made for this run:")
		fmt.Printf("    %s\n", cfg.AdminToken)
		fmt.Println("  (put ADMIN_TOKEN=... in your .env file to choose your own)")
	}
	fmt.Println()
}
