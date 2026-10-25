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
	"github.com/MacroXie04/Berth/internal/docker"
	"github.com/MacroXie04/Berth/internal/reaper"
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/webui"
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

	dock, err := docker.NewMoby()
	if err != nil {
		return err
	}
	defer dock.Close()
	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	if err := dock.Ping(pingCtx); err != nil {
		// Not fatal: accounts and login work without Docker, and the daemon may
		// simply come up after us.
		log.Warn("Docker is not reachable; workspace endpoints will fail until it is", "err", err)
	} else if cfg.TraefikContainer != "" && !dock.SupportsGatewayPriority() {
		log.Error("this Docker Engine is older than 28 (API < 1.48) and ignores the gateway priority Berth " +
			"sets when it attaches Traefik to a user network: the first power-on of each new user will reset " +
			"the connections of everyone currently using Traefik. Upgrade Docker Engine to 28 or newer.")
	}
	cancelPing()

	workspaces, err := newWorkspaceManager(cfg, st, dock, log)
	if err != nil {
		return err
	}
	if msg := shortIdleTimeoutWarning(cfg); msg != "" {
		log.Warn(msg)
	}
	rp, err := reaper.New(st, workspaces, dock, reaperConfig(cfg), log, nil)
	if err != nil {
		return err
	}
	stopReaper := startReaper(ctx, rp, log)
	defer stopReaper() // before dock.Close and st.Close: the reaper uses both

	web, err := webui.Embedded()
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
		Workspaces:        workspaces,
		Web:               web,
	})
	// The slowest handlers are a workspace start (bounded by StartTimeout) and a
	// stop (graceful timeout plus slack); leave headroom after both.
	writeTimeout := max(cfg.StartTimeout, cfg.StopTimeout+30*time.Second) + 15*time.Second
	httpSrv := api.NewHTTPServer(cfg.ListenAddr, srv.Handler(), writeTimeout)

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

// startReaper runs the reaper in the background and returns the function that
// stops it: it cancels the reaper's context, which aborts the Docker call in
// progress (every operation is safe to cut short: the database follows Docker on
// the next pass), and waits a bounded time for it to finish, so the database is
// not closed under a pass that is still writing to it. Eight seconds: Docker
// Compose kills the container after ten.
func startReaper(ctx context.Context, rp *reaper.Reaper, log *slog.Logger) (stop func()) {
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rp.Run(rctx)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			log.Warn("the reaper did not stop within 8 s; closing anyway")
		}
	}
}
