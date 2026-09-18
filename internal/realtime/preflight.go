package realtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrBadKey means the credential was actively rejected. Nothing will fix itself, so
// callers should treat it as fatal rather than retrying.
var ErrBadKey = errors.New("OpenAI rejected the API key")

// Preflight verifies the API key before any caller depends on it.
//
// Without this the first indication of a bad key is a caller hearing ringing followed
// by a dead line, because the Realtime session is only established once a call is
// already up. It deliberately distinguishes a rejected key (fatal: the service should
// not pretend to answer calls) from a transient network or server fault (survivable:
// the key may be fine and the API back by the time someone calls).
func Preflight(ctx context.Context, apiKey string) error {
	if looksLikePlaceholder(apiKey) {
		return fmt.Errorf("%w: the key looks like the placeholder from .env.example, "+
			"not a real credential", ErrBadKey)
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Could not reach OpenAI at all. Not evidence about the key.
		return fmt.Errorf("could not reach the OpenAI API: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (%s)", ErrBadKey, resp.Status)
	case resp.StatusCode >= 500:
		return fmt.Errorf("OpenAI API returned %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("unexpected response from the OpenAI API: %s", resp.Status)
	}
	return nil
}

func looksLikePlaceholder(key string) bool {
	k := strings.TrimSpace(key)
	// "sk-..." is what ships in .env.example; anything that short is not a real key.
	return strings.HasSuffix(k, "...") || len(k) < 20
}
