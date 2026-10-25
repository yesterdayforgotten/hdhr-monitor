// Command hdhr-monitor serves a web page for watching HDHomeRun tuner signal
// and changing channels, built for aiming an antenna from a phone.
//
// Configuration is by environment variable:
//
//	HDHR_DEVICES       comma-separated device IPs or hostnames (required)
//	HDHR_LISTEN        listen address (default :8089)
//	HDHR_LEASE         how long a tuner stays taken after its page stops
//	                   polling (default 60s)
//	HDHR_TRUSTED_NETS  comma-separated CIDRs that need no login (default:
//	                   private and loopback ranges)
//	HDHR_TRUSTED_PROXIES comma-separated reverse proxy IPs or CIDRs whose
//	                   X-Forwarded-For is believed, so LAN users coming
//	                   through them need no login (default none)
//	HDHR_AUTH          set to off to let every request in, when a proxy or
//	                   firewall in front already limits who can connect
//	HDHR_USERNAME      login for everything else (default admin)
//	HDHR_PASSWORD      password for everything else; unset refuses it
//	HDHR_GUIDE         set to 0 to not fetch network names and logos from
//	                   Silicondust's guide service (default on)
//	HDHR_DATA_DIR      where favorites are saved (default /data; empty keeps
//	                   them in memory)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yesterdayforgotten/hdhr-monitor/internal/server"
	"github.com/yesterdayforgotten/hdhr-monitor/web"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = ""

func main() {
	healthcheck := flag.Bool("healthcheck", false, "check that a running server answers, for container health checks")
	flag.Parse()

	listen := env("HDHR_LISTEN", ":8089")
	if *healthcheck {
		os.Exit(checkHealth(listen))
	}

	cfg, err := config(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hdhr-monitor:", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(cfg, web.FS)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hdhr-monitor:", err)
		os.Exit(1)
	}
	httpSrv := &http.Server{Addr: listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}

	done := make(chan struct{})
	go func() {
		srv.Run(ctx) // releases our tuners once ctx ends
		close(done)
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdown)
	}()

	slog.Info("listening", "version", cfg.Version, "addr", listen, "devices", cfg.Hosts, "auth", !cfg.NoAuth, "remote_login", cfg.Password != "")
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
	<-done
}

func config(listen string) (server.Config, error) {
	cfg := server.Config{
		CacheTTL:    350 * time.Millisecond,
		LeaseTTL:    60 * time.Second,
		Timeout:     1500 * time.Millisecond,
		TrustedNets: server.DefaultTrustedNets,
		Username:    env("HDHR_USERNAME", "admin"),
		Password:    os.Getenv("HDHR_PASSWORD"),
		DataDir:     "/data",
		Version:     version,
		Guide:       os.Getenv("HDHR_GUIDE") != "0",
	}
	switch v := os.Getenv("HDHR_AUTH"); v {
	case "", "on":
	case "off":
		cfg.NoAuth = true
	default:
		return cfg, fmt.Errorf("HDHR_AUTH %q: want on or off", v)
	}
	if cfg.Version == "" {
		// Development build: every start is a new version.
		cfg.Version = "dev-" + time.Now().UTC().Format("20060102-150405")
	}
	if v, ok := os.LookupEnv("HDHR_DATA_DIR"); ok {
		cfg.DataDir = v
	}
	cfg.Hosts = list(os.Getenv("HDHR_DEVICES"))
	if len(cfg.Hosts) == 0 {
		return cfg, errors.New("set HDHR_DEVICES to your HDHomeRun IPs, for example HDHR_DEVICES=10.10.0.54")
	}
	if v := os.Getenv("HDHR_LEASE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Second {
			return cfg, fmt.Errorf("HDHR_LEASE %q: want a duration of at least 10s", v)
		}
		cfg.LeaseTTL = d
	}
	if v := os.Getenv("HDHR_TRUSTED_NETS"); v != "" {
		nets, err := prefixes(v)
		if err != nil {
			return cfg, fmt.Errorf("HDHR_TRUSTED_NETS: %w", err)
		}
		cfg.TrustedNets = nets
	}
	proxies, err := prefixes(os.Getenv("HDHR_TRUSTED_PROXIES"))
	if err != nil {
		return cfg, fmt.Errorf("HDHR_TRUSTED_PROXIES: %w", err)
	}
	cfg.TrustedProxies = proxies
	return cfg, nil
}

// prefixes parses a comma-separated list of CIDRs and bare addresses.
func prefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range list(s) {
		if !strings.Contains(f, "/") {
			addr, err := netip.ParseAddr(f)
			if err != nil {
				return nil, err
			}
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func checkHealth(listen string) int {
	_, port, _ := strings.Cut(listen, ":")
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func list(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
