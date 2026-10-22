// Command berth is the Berth backend: one process serving the HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MacroXie04/Berth/internal/api"
	"github.com/MacroXie04/Berth/internal/auth"
	"github.com/MacroXie04/Berth/internal/config"
	"github.com/MacroXie04/Berth/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "berth:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	hasher, err := auth.NewHasher(cfg.BcryptCost)
	if err != nil {
		return err
	}

	srv := api.New(api.Deps{
		Store:             st,
		Hasher:            hasher,
		Log:               log,
		SessionTTL:        cfg.SessionTTL,
		CookieSecure:      cfg.CookieSecure,
		DefaultMemLimitMB: cfg.DefaultMemLimitMB,
		DefaultCPULimit:   cfg.DefaultCPULimit,
	})
	httpSrv := api.NewHTTPServer(cfg.ListenAddr, srv.Handler())

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "db", cfg.DBPath)
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
