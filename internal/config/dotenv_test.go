package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnv(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The bug this whole file exists for: an earlier `set -a && . ./.env` leaves a stale
// value exported, the file is edited, and the old value silently keeps winning.
func TestDotEnvOverridesStaleExportedValue(t *testing.T) {
	t.Setenv("PHONELLM_TEST_KEY", "sk-...")
	path := writeEnv(t, "PHONELLM_TEST_KEY=sk-real-credential-value\n")

	n, err := LoadDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("applied %d settings, want 1", n)
	}
	if got := os.Getenv("PHONELLM_TEST_KEY"); got != "sk-real-credential-value" {
		t.Errorf("stale exported value survived: %q", got)
	}
}

func TestDotEnvParsing(t *testing.T) {
	path := writeEnv(t, strings.Join([]string{
		"# a comment",
		"",
		"  PLAIN=value",
		`QUOTED="spaced value"`,
		"SINGLE='single quoted'",
		"export EXPORTED=exported-value",
		"TRAILING=value # with a comment",
		"HASHKEY=sk-abc#def",
		"EMPTY=",
		"URL=https://example.com/path?a=b",
	}, "\n"))

	if _, err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"PLAIN":    "value",
		"QUOTED":   "spaced value",
		"SINGLE":   "single quoted",
		"EXPORTED": "exported-value",
		"TRAILING": "value",
		// A '#' with no preceding space is part of the value: secrets contain punctuation.
		"HASHKEY": "sk-abc#def",
		"EMPTY":   "",
		// '=' inside a value must survive.
		"URL": "https://example.com/path?a=b",
	}
	for k := range want {
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestDotEnvMissingFileIsNotAnError(t *testing.T) {
	n, err := LoadDotEnv(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Errorf("a missing .env should be fine (exporting directly stays valid): %v", err)
	}
	if n != 0 {
		t.Errorf("applied %d settings from a missing file", n)
	}
}

func TestDotEnvReportsMalformedLine(t *testing.T) {
	path := writeEnv(t, "GOOD=value\nthis line has no equals sign\n")
	_, err := LoadDotEnv(path)
	if err == nil {
		t.Fatal("expected an error for a malformed line")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should point at the offending line, got: %v", err)
	}
}

func TestFingerprintNeverRevealsTheSecret(t *testing.T) {
	secret := "sk-svcacct-47m0PabcdefghijklmnopqrstuvwxyzXYZ"
	got := Fingerprint(secret)

	if strings.Contains(got, "abcdefghijklmnop") {
		t.Errorf("fingerprint leaked the middle of the secret: %q", got)
	}
	if !strings.Contains(got, "sk-svcac") {
		t.Errorf("fingerprint should show enough to identify the key, got %q", got)
	}
	if Fingerprint("") != "(empty)" {
		t.Errorf("empty secret = %q", Fingerprint(""))
	}
	if !strings.Contains(Fingerprint("sk-..."), "too short") {
		t.Errorf("placeholder should be called out, got %q", Fingerprint("sk-..."))
	}
}
