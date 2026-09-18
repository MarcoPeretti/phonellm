// Package config loads phonellm's runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultInstructions = `You are a polite, concise telephone answering assistant.
You are answering the household's landline because nobody is available to pick up.
Greet the caller, tell them you are an automated assistant, and ask who is calling and
what the call is about. If they want a call back, ask for a number and repeat it back to
confirm. Keep every reply to one or two short sentences: this is a phone call, not a
chat. Never invent facts about the household's schedule or whereabouts. If the caller
sounds like a salesperson, politely decline and end the call.`

type Config struct {
	// SIP leg (the Fritz!Box acts as registrar and PBX).
	SIPUsername  string
	SIPPassword  string
	SIPRegistrar string
	BindHost     string
	BindPort     int
	RTPPortMin   int
	RTPPortMax   int

	// OpenAI Realtime leg.
	APIKey       string
	Model        string
	Voice        string
	Instructions string

	// Call behaviour.
	RingDelay      time.Duration
	MaxCallTime    time.Duration
	SilenceTimeout time.Duration
	EchoTest       bool

	// Artefacts.
	RecordDir     string
	TranscriptDir string

	// Notification (all optional).
	SummaryModel  string
	TelegramToken string
	TelegramChat  string
	SMTPHost      string
	SMTPPort      int
	SMTPUser      string
	SMTPPass      string
	SMTPFrom      string
	SMTPTo        string
}

// Load reads configuration from the environment, applying defaults. It returns an
// error listing every missing required value at once rather than failing one at a time.
func Load() (*Config, error) {
	c := &Config{
		SIPUsername:    os.Getenv("PHONELLM_SIP_USER"),
		SIPPassword:    os.Getenv("PHONELLM_SIP_PASS"),
		SIPRegistrar:   env("PHONELLM_SIP_REGISTRAR", "fritz.box"),
		BindHost:       env("PHONELLM_BIND_HOST", ""),
		BindPort:       envInt("PHONELLM_BIND_PORT", 5060),
		RTPPortMin:     envInt("PHONELLM_RTP_PORT_MIN", 16384),
		RTPPortMax:     envInt("PHONELLM_RTP_PORT_MAX", 16484),
		APIKey:         os.Getenv("OPENAI_API_KEY"),
		Model:          env("PHONELLM_MODEL", "gpt-realtime"),
		Voice:          env("PHONELLM_VOICE", "alloy"),
		RingDelay:      envDur("PHONELLM_RING_DELAY", 2*time.Second),
		MaxCallTime:    envDur("PHONELLM_MAX_CALL_TIME", 5*time.Minute),
		SilenceTimeout: envDur("PHONELLM_SILENCE_TIMEOUT", 30*time.Second),
		EchoTest:       envBool("PHONELLM_ECHO_TEST", false),
		RecordDir:      env("PHONELLM_RECORD_DIR", ""),
		TranscriptDir:  env("PHONELLM_TRANSCRIPT_DIR", ""),
		SummaryModel:   env("PHONELLM_SUMMARY_MODEL", "gpt-4.1-mini"),
		TelegramToken:  os.Getenv("PHONELLM_TELEGRAM_TOKEN"),
		TelegramChat:   os.Getenv("PHONELLM_TELEGRAM_CHAT_ID"),
		SMTPHost:       os.Getenv("PHONELLM_SMTP_HOST"),
		SMTPPort:       envInt("PHONELLM_SMTP_PORT", 587),
		SMTPUser:       os.Getenv("PHONELLM_SMTP_USER"),
		SMTPPass:       os.Getenv("PHONELLM_SMTP_PASS"),
		SMTPFrom:       os.Getenv("PHONELLM_SMTP_FROM"),
		SMTPTo:         os.Getenv("PHONELLM_SMTP_TO"),
	}

	c.Instructions = defaultInstructions
	if path := os.Getenv("PHONELLM_INSTRUCTIONS_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading PHONELLM_INSTRUCTIONS_FILE: %w", err)
		}
		c.Instructions = strings.TrimSpace(string(b))
	} else if s := os.Getenv("PHONELLM_INSTRUCTIONS"); s != "" {
		c.Instructions = s
	}

	var missing []string
	if c.SIPUsername == "" {
		missing = append(missing, "PHONELLM_SIP_USER")
	}
	if c.SIPPassword == "" {
		missing = append(missing, "PHONELLM_SIP_PASS")
	}
	// The echo test deliberately needs no OpenAI credentials: it is the milestone that
	// proves the SIP and RTP path in isolation, before any LLM is involved.
	if c.APIKey == "" && !c.EchoTest {
		missing = append(missing, "OPENAI_API_KEY")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
