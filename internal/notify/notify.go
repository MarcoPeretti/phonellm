// Package notify turns a finished call into something that reaches the user: a stored
// transcript plus a short summary pushed over Telegram and/or email.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcoperetti/phonellm/internal/bridge"
	"github.com/marcoperetti/phonellm/internal/config"
)

type Notifier struct {
	cfg *config.Config
	log *slog.Logger
	hc  *http.Client
}

func New(cfg *config.Config, log *slog.Logger) *Notifier {
	return &Notifier{cfg: cfg, log: log, hc: &http.Client{Timeout: 30 * time.Second}}
}

// Handle is the CallHandler passed to the telephony agent. Every step is independent
// and best-effort: a failing Telegram token must not cost you the transcript on disk.
func (n *Notifier) Handle(ctx context.Context, caller string, res bridge.Result) {
	transcript := res.Transcript()

	if err := n.save(caller, res, transcript); err != nil {
		n.log.Error("saving transcript", "error", err)
	}

	summary := n.summarize(ctx, caller, res, transcript)
	body := fmt.Sprintf("Call from %s (%s, ended: %s)\n\n%s\n\n--- transcript ---\n%s",
		display(caller), res.Duration.Round(time.Second), res.Reason, summary, transcript)

	if n.cfg.TelegramToken != "" && n.cfg.TelegramChat != "" {
		if err := n.telegram(ctx, body); err != nil {
			n.log.Error("telegram notification failed", "error", err)
		}
	}
	if n.cfg.SMTPHost != "" && n.cfg.SMTPTo != "" {
		if err := n.email(fmt.Sprintf("Call from %s", display(caller)), body); err != nil {
			n.log.Error("email notification failed", "error", err)
		}
	}
}

func (n *Notifier) save(caller string, res bridge.Result, transcript string) error {
	if n.cfg.TranscriptDir == "" {
		return nil
	}
	if err := os.MkdirAll(n.cfg.TranscriptDir, 0o750); err != nil {
		return err
	}
	name := fmt.Sprintf("%s_%s.txt", time.Now().Format("20060102-150405"), sanitize(caller))
	content := fmt.Sprintf("caller: %s\nduration: %s\nreason: %s\n\n%s",
		display(caller), res.Duration.Round(time.Second), res.Reason, transcript)
	return os.WriteFile(filepath.Join(n.cfg.TranscriptDir, name), []byte(content), 0o640)
}

// summarize asks a cheap text model to compress the call. On any failure it degrades to
// the raw transcript rather than losing the message entirely.
func (n *Notifier) summarize(ctx context.Context, caller string, res bridge.Result, transcript string) string {
	if n.cfg.APIKey == "" || strings.TrimSpace(transcript) == "" {
		return "(no summary)"
	}

	prompt := fmt.Sprintf(
		"Summarise this answering-machine call in at most three short lines: who called, "+
			"what they wanted, and any callback number or action needed. "+
			"If nothing useful was said, say so in one line.\n\nCaller ID: %s\n\n%s",
		display(caller), transcript)

	payload, err := json.Marshal(map[string]any{
		"model": n.cfg.SummaryModel,
		"input": prompt,
	})
	if err != nil {
		return transcript
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.openai.com/v1/responses", bytes.NewReader(payload))
	if err != nil {
		return transcript
	}
	req.Header.Set("Authorization", "Bearer "+n.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.hc.Do(req)
	if err != nil {
		n.log.Error("summary request failed", "error", err)
		return transcript
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		n.log.Error("summary request rejected", "status", resp.Status)
		return transcript
	}

	var out struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return transcript
	}
	if s := strings.TrimSpace(out.OutputText); s != "" {
		return s
	}
	for _, o := range out.Output {
		for _, c := range o.Content {
			if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
				return strings.TrimSpace(c.Text)
			}
		}
	}
	return transcript
}

func (n *Notifier) telegram(ctx context.Context, body string) error {
	payload, err := json.Marshal(map[string]any{
		"chat_id": n.cfg.TelegramChat,
		"text":    truncate(body, 4000),
	})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.TelegramToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram returned %s", resp.Status)
	}
	return nil
}

func (n *Notifier) email(subject, body string) error {
	from := n.cfg.SMTPFrom
	if from == "" {
		from = n.cfg.SMTPUser
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		from, n.cfg.SMTPTo, subject, body)

	addr := fmt.Sprintf("%s:%d", n.cfg.SMTPHost, n.cfg.SMTPPort)
	var auth smtp.Auth
	if n.cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", n.cfg.SMTPUser, n.cfg.SMTPPass, n.cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, from, strings.Split(n.cfg.SMTPTo, ","), []byte(msg))
}

func display(caller string) string {
	if caller == "" {
		return "withheld number"
	}
	return caller
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func sanitize(s string) string {
	if s == "" {
		return "unknown"
	}
	out := []rune(s)
	for i, r := range out {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			out[i] = '_'
		}
	}
	return string(out)
}
