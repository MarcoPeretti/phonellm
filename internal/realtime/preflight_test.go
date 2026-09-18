package realtime

import (
	"context"
	"errors"
	"testing"
)

func TestLooksLikePlaceholder(t *testing.T) {
	placeholders := []string{
		"sk-...",      // straight from .env.example
		"sk-proj-...", //
		"  sk-...  ",  // whitespace from a sloppy env file
		"",            //
		"sk-short",    //
	}
	for _, k := range placeholders {
		if !looksLikePlaceholder(k) {
			t.Errorf("%q should be treated as a placeholder", k)
		}
	}

	real := "sk-proj-" + string(make([]byte, 48))
	if looksLikePlaceholder(real) {
		t.Errorf("a full-length key was mistaken for a placeholder")
	}
}

// The placeholder check must short-circuit before any network call, so this stays
// offline and still exercises the path the user actually hit.
func TestPreflightRejectsPlaceholderAsBadKey(t *testing.T) {
	err := Preflight(context.Background(), "sk-...")
	if err == nil {
		t.Fatal("expected the placeholder key to be rejected")
	}
	if !errors.Is(err, ErrBadKey) {
		t.Errorf("error should be ErrBadKey so startup treats it as fatal, got: %v", err)
	}
}
