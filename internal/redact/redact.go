// Package redact removes secrets and personal paths from text meant to
// leave the machine (exports, footprints).
package redact

import (
	"os"
	"os/user"
	"regexp"
	"strings"
)

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\b[sr]k_(live|test)_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
}

// assignment catches key=value / key: value where the key names a secret.
var assignment = regexp.MustCompile(`(?i)((?:api[_-]?key|auth[_-]?token|_authToken|access[_-]?token|token|secret|passw(?:or)?d|pwd)["']?\s*[:=]\s*["']?)([^\s"']{6,})`)

// URL credentials: scheme://user:password@host
var urlCreds = regexp.MustCompile(`([a-z][a-z0-9+.-]*://[^/\s:@]+:)([^@\s/]+)(@)`)

// Text returns s with secrets replaced by [REDACTED] and the home
// directory and user name generalised. n counts secret replacements.
func Text(s string) (string, int) {
	n := 0
	for _, re := range patterns {
		s = re.ReplaceAllStringFunc(s, func(string) string { n++; return "[REDACTED]" })
	}
	s = assignment.ReplaceAllStringFunc(s, func(m string) string {
		sub := assignment.FindStringSubmatch(m)
		if sub[2] == "[REDACTED]" {
			return m
		}
		n++
		return sub[1] + "[REDACTED]"
	})
	s = urlCreds.ReplaceAllStringFunc(s, func(m string) string {
		n++
		sub := urlCreds.FindStringSubmatch(m)
		return sub[1] + "[REDACTED]" + sub[3]
	})
	return Paths(s), n
}

// Paths replaces the home directory with ~ and the user name with $USER.
func Paths(s string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "/" {
		s = strings.ReplaceAll(s, home, "~")
	}
	if u, err := user.Current(); err == nil && len(u.Username) > 2 {
		s = regexp.MustCompile(`\b`+regexp.QuoteMeta(u.Username)+`\b`).ReplaceAllString(s, "$$USER")
	}
	return s
}
