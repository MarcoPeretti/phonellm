// Command phonellm answers the household landline with a voice LLM.
//
// It registers with a Fritz!Box as a LAN IP telephone, answers the numbers the box
// routes to it, and bridges G.711 audio straight to the OpenAI Realtime API. No PBX,
// nothing exposed to the internet.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/MarcoPeretti/phonellm/internal/config"
	"github.com/MarcoPeretti/phonellm/internal/notify"
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

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger()

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

	if cfg.EchoTest {
		log.Warn("starting in ECHO TEST mode: calls are echoed back, the LLM is not used")
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
