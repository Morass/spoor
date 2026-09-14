package redact

import (
	"os"
	"strings"
	"testing"
)

func TestSecretsRedacted(t *testing.T) {
	secrets := []string{
		"export GH=ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"aws_access_key_id = AKIAABCDEFGHIJKLMNOP",
		"//registry.npmjs.org/:_authToken=npm_abcdefghijklmnopqrstuvwxyz0123456789",
		"ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuv",
		"password: hunter2hunter2",
		"git clone https://bob:s3cretpass@example.com/r.git",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----",
		"Authorization: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
	}
	leaks := []string{"ghp_abc", "AKIAABCD", "npm_abc", "sk-ant-api03", "hunter2", "s3cretpass", "b3BlbnNzaC1rZXk", "dozjgNry"}
	for i, s := range secrets {
		out, n := Text(s)
		if n == 0 || strings.Contains(out, leaks[i]) {
			t.Errorf("not redacted (%d): %q -> %q", n, s, out)
		}
	}
}

func TestPlainTextUntouched(t *testing.T) {
	s := `export PATH="/opt/foo/bin:$PATH"` + "\nalias ll='ls -l'\n"
	if out, n := Text(s); n != 0 || out != s {
		t.Errorf("changed innocent text (%d): %q", n, out)
	}
}

func TestHomeGeneralised(t *testing.T) {
	home, _ := os.UserHomeDir()
	out, _ := Text("source " + home + "/.cargo/env")
	if strings.Contains(out, home) || !strings.Contains(out, "~/.cargo/env") {
		t.Errorf("home not generalised: %q", out)
	}
}
