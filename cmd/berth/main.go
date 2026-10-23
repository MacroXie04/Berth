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
	"github.com/MacroXie04/Berth/internal/store"
	"github.com/MacroXie04/Berth/internal/workspace"
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
	}
	cancelPing()

	workspaces, err := workspace.NewManager(st, dock, workspace.Config{
		Image:                cfg.WorkspaceImage,
		TraefikContainer:     cfg.TraefikContainer,
		Entrypoint:           cfg.TraefikEntrypoint,
		UDPPortMin:           cfg.UDPPortMin,
		UDPPortMax:           cfg.UDPPortMax,
		UDPPortsPerWorkspace: cfg.UDPPortsPerWorkspace,
		Resolution:           cfg.Resolution,
		PublicIP:             cfg.PublicIP,
		ShmSizeMB:            cfg.ShmSizeMB,
		PidsLimit:            int64(cfg.PidsLimit),
		StopTimeout:          cfg.StopTimeout,
		StartTimeout:         cfg.StartTimeout,
	}, log, nil)
	if err != nil {
		return err
	}
	go keepProxyAttached(ctx, workspaces, log, time.Minute)

	srv := api.New(api.Deps{
		Store:             st,
		Hasher:            hasher,
		Log:               log,
		SessionTTL:        cfg.SessionTTL,
		CookieSecure:      cfg.CookieSecure,
		DefaultMemLimitMB: cfg.DefaultMemLimitMB,
		DefaultCPULimit:   cfg.DefaultCPULimit,
		Workspaces:        workspaces,
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

// keepProxyAttached reconnects Traefik to every user network at startup and then
// periodically. Docker drops those attachments whenever the Traefik container is
// recreated; without this every workspace would answer 504 until its owner
// happened to restart it. A failure is logged when it first appears and again
// when it clears, not every interval.
func keepProxyAttached(ctx context.Context, m *workspace.Manager, log *slog.Logger, every time.Duration) {
	var lastErr string
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		attachCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := m.ReattachTraefik(attachCtx)
		cancel()
		switch {
		case err != nil && err.Error() != lastErr:
			log.Warn("could not attach the proxy to user networks", "err", err)
			lastErr = err.Error()
		case err == nil && lastErr != "":
			log.Info("proxy attached to all user networks again")
			lastErr = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
