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
	// AllowPublicRegistrar permits registering to a registrar outside the private
	// address ranges, for an external SIP provider.
	AllowPublicRegistrar bool
	BindPort             int
	RTPPortMin           int
	RTPPortMax           int

	// OpenAI Realtime leg.
	APIKey string
	Model  string
	Voice  string
	// Server-VAD tuning. Raising the threshold or the silence window makes the model
	// less likely to mistake its own echoed voice for the caller interrupting.
	VADThreshold float64
	VADSilenceMS int
	VADPrefixMS  int
	Instructions string

	// Call behaviour.
	RingDelay      time.Duration
	MaxCallTime    time.Duration
	SilenceTimeout time.Duration
	EchoTest       bool
	// OutputBuffer bounds the pacing buffer between the model and the RTP clock. The
	// Realtime API streams a whole reply's audio faster than realtime, so this must be
	// large enough to hold a full reply or the oldest bytes are dropped mid-utterance
	// and the caller hears choppy speech.
	OutputBuffer time.Duration
	// OutboundTest, with OutboundNumber, places one outbound call on startup once the
	// agent has registered. It is a test hook: general outbound calling is not a feature.
	OutboundTest   bool
	OutboundNumber string

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
		// Server-VAD defaults mirror OpenAI's own server_vad defaults. A zero threshold
		// makes turn detection fire on any line energy, which slices the caller into
		// spurious turns and floods the pacing buffer; these must be wired, not left at
		// the zero value.
		VADThreshold: envFloat("PHONELLM_VAD_THRESHOLD", 0.5),
		VADSilenceMS: envInt("PHONELLM_VAD_SILENCE_MS", 500),
		VADPrefixMS:  envInt("PHONELLM_VAD_PREFIX_MS", 300),
		RingDelay:      envDur("PHONELLM_RING_DELAY", 2*time.Second),
		MaxCallTime:    envDur("PHONELLM_MAX_CALL_TIME", 5*time.Minute),
		SilenceTimeout: envDur("PHONELLM_SILENCE_TIMEOUT", 30*time.Second),
		EchoTest:       envBool("PHONELLM_ECHO_TEST", false),
		OutputBuffer:   envDur("PHONELLM_OUTPUT_BUFFER", 10*time.Second),
		OutboundTest:   envBool("OUTBOUND_TEST_CALL", false),
		OutboundNumber: os.Getenv("OUTBOUND_TEST_NUMBER"),
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

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
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
