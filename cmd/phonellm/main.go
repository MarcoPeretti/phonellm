// Command phonellm answers the household landline with a voice LLM.
//
// It registers with a Fritz!Box as a LAN IP telephone, answers the numbers the box
// routes to it, and bridges G.711 audio straight to the OpenAI Realtime API. No PBX,
// nothing exposed to the internet.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/MarcoPeretti/phonellm/internal/config"
	"github.com/MarcoPeretti/phonellm/internal/notify"
	"github.com/MarcoPeretti/phonellm/internal/realtime"
	"github.com/MarcoPeretti/phonellm/internal/telephony"
)

func main() {
	if err := run(); err != nil {
		slog.Error("phonellm exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// An explicit flag beats the config file, which beats the shell. Echo mode needs
	// this: it is a deliberate one-off override, and .env must not silently win over it.
	echo := flag.Bool("echo", false, "echo caller audio back instead of using the LLM")
	flag.Parse()

	// Read .env before anything else. The file wins over already-exported variables so
	// that editing it is always sufficient; see config.LoadDotEnv.
	envFile := os.Getenv("PHONELLM_ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	applied, err := config.LoadDotEnv(envFile)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if *echo {
		cfg.EchoTest = true
	}

	log := newLogger()
	if applied > 0 {
		log.Info("loaded configuration file", "path", envFile, "settings", applied)
	}

	// SIP carries the signalling address inside the message body, so binding to the
	// wildcard address produces an unreachable Contact. Resolve a concrete LAN address.
	if cfg.BindHost == "" {
		ip, err := outboundIP(cfg.SIPRegistrar)
		if err != nil {
			return fmt.Errorf("could not determine the LAN address to bind to; "+
				"set PHONELLM_BIND_HOST explicitly: %w", err)
		}
		cfg.BindHost = ip
		log.Info("auto-detected bind address", "host", ip)
	}

	// Resolve the registrar before doing anything else: a name that resolves off-LAN
	// is the difference between "it works" and a silent transaction timeout.
	ips, err := telephony.ResolveRegistrar(cfg.SIPRegistrar, cfg.AllowPublicRegistrar)
	if err != nil {
		return err
	}
	log.Info("registrar resolved", "host", cfg.SIPRegistrar, "addresses", ips)

	if cfg.EchoTest {
		log.Warn("starting in ECHO TEST mode: calls are echoed back, the LLM is not used")
	} else {
		// Check the credential now rather than when a caller is already on the line.
		// A rejected key is fatal: refusing to start leaves the Fritz!Box answering
		// machine to take calls, which beats answering them into a dead session.
		if err := realtime.Preflight(ctx, cfg.APIKey); err != nil {
			if errors.Is(err, realtime.ErrBadKey) {
				return fmt.Errorf("%w [effective key: %s, from %s]",
					err, config.Fingerprint(cfg.APIKey), envFile)
			}
			log.Warn("could not verify the OpenAI API key at startup; continuing", "error", err)
		} else {
			log.Info("OpenAI API key verified", "key", config.Fingerprint(cfg.APIKey))
		}
	}

	notifier := notify.New(cfg, log)
	agent, err := telephony.NewAgent(cfg, log, notifier.Handle)
	if err != nil {
		return err
	}

	log.Info("phonellm starting",
		"registrar", cfg.SIPRegistrar,
		"user", cfg.SIPUsername,
		"sip", fmt.Sprintf("%s:%d", cfg.BindHost, cfg.BindPort),
		"rtp", fmt.Sprintf("%d-%d", cfg.RTPPortMin, cfg.RTPPortMax),
		"model", cfg.Model,
	)

	return agent.Run(ctx)
}

// outboundIP asks the kernel which local address it would use to reach the registrar.
// A UDP "connection" performs no traffic, it only consults the routing table.
func outboundIP(registrar string) (string, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(hostOnly(registrar), "5060"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	return host, err
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if v, ok := os.LookupEnv("PHONELLM_LOG_LEVEL"); ok {
		_ = level.UnmarshalText([]byte(v))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
