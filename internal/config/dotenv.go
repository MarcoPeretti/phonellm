package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadDotEnv reads a .env file and applies it to the process environment.
//
// Values in the file take precedence over variables already exported in the shell.
// That is the opposite of the usual dotenv convention, and it is deliberate: the
// failure it prevents is editing .env, running the service, and silently getting a
// stale value that an earlier `set -a && . ./.env` left behind in the shell. For a
// single-operator service the file is the configuration, so the file wins.
//
// A missing file is not an error: exporting the variables directly stays valid.
func LoadDotEnv(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	var applied int
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		key, value, ok, err := parseLine(scanner.Text())
		if err != nil {
			return applied, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if !ok {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, scanner.Err()
}

func parseLine(raw string) (key, value string, ok bool, err error) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	line = strings.TrimPrefix(line, "export ")

	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false, fmt.Errorf("expected KEY=VALUE, got %q", raw)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", false, fmt.Errorf("empty key in %q", raw)
	}

	value = strings.TrimSpace(value)
	// Strip one layer of matching quotes; leave the contents alone otherwise, since
	// API keys and passwords may legitimately contain '#' and other punctuation.
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			return key, value[1 : len(value)-1], true, nil
		}
	}
	// An unquoted trailing comment is the one case worth handling, and only when the
	// '#' is clearly separated from the value.
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return key, value, true, nil
}

// Fingerprint renders a secret safely for logs: enough to tell two keys apart, not
// enough to use. This exists so a mismatch between the intended and the effective
// credential is visible without ever printing the credential.
func Fingerprint(secret string) string {
	s := strings.TrimSpace(secret)
	if s == "" {
		return "(empty)"
	}
	if len(s) <= 12 {
		return "(too short to be valid)"
	}
	return fmt.Sprintf("%s...%s (%d chars)", s[:8], s[len(s)-4:], len(s))
}
