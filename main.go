// Command caldav-gateway serves a filtered, read-only ICS feed from a
// CalDAV backend behind a secret, unauthenticated URL path.
package main

import (
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"go.nadia.moe/calshare/gateway"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, listenAddr := loadConfig()
	client := &http.Client{Timeout: 10 * time.Second}

	gw := gateway.New(cfg, client, logger)

	slog.Info("listening", "addr", listenAddr)
	if err := http.ListenAndServe(listenAddr, gw); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}

func loadConfig() (gateway.Config, string) {
	cfg := gateway.Config{
		SecretPath:  mustEnv("SECRET_PATH"),
		BackendURL:  mustEnv("BACKEND_URL"),
		BackendUser: mustEnv("BACKEND_USER"),
		BackendPass: mustEnv("BACKEND_PASS"),
	}

	if v := os.Getenv("ALLOWLIST"); v != "" {
		cfg.Allow = compileEnvRegexp("ALLOWLIST", v)
	}
	if v := os.Getenv("DENYLIST"); v != "" {
		cfg.Deny = compileEnvRegexp("DENYLIST", v)
	}
	cfg.Anonymize = os.Getenv("ANONYMIZE")

	if !strings.HasPrefix(cfg.SecretPath, "/") {
		cfg.SecretPath = "/" + cfg.SecretPath
	}

	return cfg, envOr("LISTEN_ADDR", ":8080")
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		slog.Error("missing required env var", "name", name)
		os.Exit(1)
	}
	return v
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func compileEnvRegexp(name, pattern string) *regexp.Regexp {
	re, err := regexp.Compile(pattern)
	if err != nil {
		slog.Error("invalid regex in env var", "name", name, "error", err)
		os.Exit(1)
	}
	return re
}
