// Command greylistd-frontend is a small web interface for greylistd(8).
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
	"runtime/debug"
	"syscall"
	"time"

	// Embed the time zone database: the binary runs in a FROM scratch
	// container without /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/jdelic/greylistd-frontend/internal/auth"
	"github.com/jdelic/greylistd-frontend/internal/config"
	"github.com/jdelic/greylistd-frontend/internal/greylistd"
	"github.com/jdelic/greylistd-frontend/internal/logging"
	"github.com/jdelic/greylistd-frontend/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

func getVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		bootLog := logging.NewApp(os.Stderr, slog.LevelInfo)
		bootLog.Error("invalid configuration", "error", err.Error())
		return 2
	}
	if cfg.ShowVersion {
		fmt.Println(getVersion())
		return 0
	}

	level, err := logging.ParseLevel(cfg.LogLevel)
	if err != nil {
		logging.NewApp(os.Stderr, slog.LevelInfo).Error("invalid configuration", "error", err.Error())
		return 2
	}
	log := logging.NewApp(os.Stderr, level)
	slog.SetDefault(log)
	access := logging.NewAccess(os.Stdout)

	if cfg.SessionSecret == nil {
		log.Warn("no session secret configured; using a random one, so all sessions end when the process restarts",
			"hint", "set "+config.EnvName("session-secret"))
	}
	signer, err := auth.NewSigner(cfg.SessionSecret)
	if err != nil {
		log.Error("cannot create session signer", "error", err)
		return 1
	}
	sessions := &auth.Sessions{Signer: signer, TTL: cfg.SessionTTL, Secure: cfg.SecureCookies()}

	gl := greylistd.New(cfg.SocketPath)
	gl.Timeout = cfg.SocketTimeout
	gl.Location = cfg.Location()

	opt := web.Options{
		Greylistd:  gl,
		Sessions:   sessions,
		OIDCName:   cfg.OIDCProviderName,
		Password:   auth.NewPasswordChecker(cfg.AuthUsername, cfg.AuthPassword),
		Log:        log,
		TrustProxy: cfg.TrustProxy,
		Version:    getVersion(),
		DisplayTZ:  cfg.Location(),
		Anonymous:  cfg.AllowAnonymous,
	}
	switch {
	case cfg.AllowAnonymous:
		log.Warn("no login required: everybody who can reach the server may manage greylistd (--allow-anonymous)")
	case !cfg.LoginConfigured():
		log.Error("no login method configured; the UI only shows an error",
			"hint", "set --oidc-issuer and/or --auth-username/--auth-password, or --allow-anonymous")
	}
	if cfg.OIDCEnabled() {
		opt.OIDC = auth.NewOIDC(auth.OIDCConfig{
			Issuer:        cfg.OIDCIssuer,
			ClientID:      cfg.OIDCClientID,
			ClientSecret:  cfg.OIDCClientSecret,
			RedirectURL:   cfg.BaseURL + "/auth/oidc/callback",
			Scopes:        cfg.OIDCScopes,
			AllowedEmails: cfg.OIDCAllowedEmails,
			AllowedGroups: cfg.OIDCAllowedGroups,
			GroupsClaim:   cfg.OIDCGroupsClaim,
			AllowAnyUser:  cfg.OIDCAllowAnyUser,
		})
		if cfg.OIDCAllowAnyUser {
			log.Warn("every user authenticated by the OIDC provider may manage greylistd (--oidc-allow-any-user)")
		}
	}

	srv, err := web.New(opt)
	if err != nil {
		log.Error("cannot initialize web server", "error", err)
		return 1
	}

	handler := logging.AccessLog(access, func(r *http.Request) string { return web.ClientIP(r, cfg.TrustProxy) }, srv)
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           http.MaxBytesHandler(handler, 1<<20),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := gl.Ping(ctx); err != nil {
		// Not fatal: greylistd may come up later; /healthz reports it.
		log.Warn("greylistd is not reachable yet", "socket", cfg.SocketPath, "error", err)
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "version", getVersion(), "socket", cfg.SocketPath,
			"oidc", cfg.OIDCEnabled(), "password_login", cfg.PasswordEnabled(), "anonymous", cfg.AllowAnonymous)
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "error", err)
			return 1
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Error("graceful shutdown failed", "error", err)
			return 1
		}
	}
	return 0
}
