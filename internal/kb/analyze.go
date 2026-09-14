package kb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	udiff "github.com/aymanbagabas/go-udiff"
	"github.com/morass/spoor/internal/diff"
	"github.com/morass/spoor/internal/model"
	"github.com/morass/spoor/internal/store"
)

// Detail is one fact worth showing next to a change.
type Detail struct {
	Key   string
	Value string
	Risk  Risk
}

// Analyze inspects the content of a change and reports what matters:
// launchd keys, systemd directives, lines added to shell files, what
// kind of binary appeared and who signed it.
func Analyze(st *store.Store, c model.Change, live bool) []Detail {
	var out []Detail
	after, _ := diff.Content(st, c.After, live)
	before, _ := diff.Content(st, c.Before, live && c.After == nil)
	body := after
	if body == nil {
		body = before
	}
	switch {
	case body != nil && diff.IsPlist(body):
		out = append(out, launchd(body)...)
	case strings.HasSuffix(c.Path, ".service") || strings.HasSuffix(c.Path, ".timer"):
		out = append(out, systemdUnit(body)...)
	}
	if body != nil && !diff.IsBinary(body) && !diff.IsPlist(body) {
		out = append(out, shellLines(string(before), string(after))...)
	}
	if strings.HasPrefix(c.Path, model.StatePrefix) {
		out = append(out, stateLines(string(before), string(after))...)
	}
	e := c.After
	if e == nil {
		e = c.Before
	}
	if e != nil && e.Type == model.File {
		out = append(out, fileKind(e, body, c.After != nil && live)...)
	}
	if e != nil && e.Skipped == "sensitive" {
		out = append(out, Detail{"content", "not stored: looks like a secret (key, token, credentials)", Notice})
	}
	return out
}

func launchd(b []byte) []Detail {
	v, err := diff.DecodePlist(b)
	if err != nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if _, isJob := m["Label"]; !isJob {
		return nil
	}
	var out []Detail
	add := func(k, val string, r Risk) { out = append(out, Detail{k, val, r}) }
	add("Label", fmt.Sprint(m["Label"]), Info)
	if p, ok := m["Program"]; ok {
		add("Program", fmt.Sprint(p), Notice)
	}
	if args, ok := m["ProgramArguments"].([]any); ok {
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = fmt.Sprint(a)
		}
		add("Runs", strings.Join(parts, " "), Notice)
	}
	if t, ok := m["RunAtLoad"].(bool); ok && t {
		add("RunAtLoad", "starts immediately when loaded and at every login/boot", Warn)
	}
	switch k := m["KeepAlive"].(type) {
	case bool:
		if k {
			add("KeepAlive", "launchd restarts it whenever it exits — killing it does not stop it", Warn)
		}
	case map[string]any:
		add("KeepAlive", "restarted under conditions: "+keys(k), Notice)
	}
	if n, ok := num(m["StartInterval"]); ok {
		add("StartInterval", "runs every "+human(time.Duration(n)*time.Second), Notice)
	}
	if ci, ok := m["StartCalendarInterval"]; ok {
		add("StartCalendarInterval", "scheduled: "+calendar(ci), Notice)
	}
	if w, ok := m["WatchPaths"].([]any); ok {
		add("WatchPaths", fmt.Sprintf("runs when any of %d paths change", len(w)), Notice)
	}
	if u, ok := m["UserName"]; ok {
		add("UserName", "runs as "+fmt.Sprint(u), Warn)
	}
	if _, ok := m["MachServices"]; ok {
		add("MachServices", "registers IPC services other processes can call", Info)
	}
	return out
}

func keys(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return strings.Join(ks, ", ")
}

func num(v any) (int64, bool) {
	switch t := v.(type) {
	case uint64:
		return int64(t), true
	case int64:
		return t, true
	case float64:
		return int64(t), true
	}
	return 0, false
}

func human(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d day(s)", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d hour(s)", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%d minute(s)", d/time.Minute)
	}
	return d.String()
}

func calendar(v any) string {
	one := func(m map[string]any) string {
		var parts []string
		for _, k := range []string{"Month", "Day", "Weekday", "Hour", "Minute"} {
			if n, ok := num(m[k]); ok {
				parts = append(parts, fmt.Sprintf("%s=%d", k, n))
			}
		}
		if len(parts) == 0 {
			return "every minute"
		}
		return strings.Join(parts, " ")
	}
	switch t := v.(type) {
	case map[string]any:
		return one(t)
	case []any:
		var s []string
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				s = append(s, one(m))
			}
		}
		return strings.Join(s, "; ")
	}
	return fmt.Sprint(v)
}

func systemdUnit(b []byte) []Detail {
	var out []Detail
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		for _, k := range []string{"ExecStart", "OnCalendar", "OnBootSec", "OnUnitActiveSec", "WantedBy", "Restart", "User"} {
			if strings.HasPrefix(line, k+"=") {
				r := Notice
				if k == "ExecStart" || k == "User" {
					r = Warn
				}
				out = append(out, Detail{k, strings.TrimPrefix(line, k+"="), r})
			}
		}
	}
	return out
}

var shellPatterns = []struct {
	re   *regexp.Regexp
	what string
	risk Risk
}{
	{regexp.MustCompile(`(curl|wget)[^|]*\|\s*(ba|z)?sh`), "downloads and runs a script on every shell start", Warn},
	{regexp.MustCompile(`\beval\s`), "evaluates generated code on every shell start", Notice},
	{regexp.MustCompile(`(^|\s)(export\s+)?PATH=`), "changes PATH", Notice},
	{regexp.MustCompile(`^\s*(source|\.)\s+\S`), "sources another file", Notice},
	{regexp.MustCompile(`^\s*alias\s`), "defines an alias", Info},
	{regexp.MustCompile(`^\s*export\s+[A-Z_]+=`), "sets an environment variable", Info},
	{regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)\s*=`), "stores something that looks like a secret", Warn},
}

// shellLines reports notable lines that were added (or removed).
func shellLines(before, after string) []Detail {
	if before == after {
		return nil
	}
	var out []Detail
	for _, e := range udiff.Lines(before, after) {
		removed := before[e.Start:e.End]
		for _, l := range strings.Split(e.New, "\n") {
			out = append(out, matchShell("added", l)...)
		}
		if after == "" {
			continue
		}
		for _, l := range strings.Split(removed, "\n") {
			out = append(out, matchShell("removed", l)...)
		}
	}
	if len(out) > 12 {
		out = append(out[:12], Detail{"…", fmt.Sprintf("%d more notable lines", len(out)-12), Info})
	}
	return out
}

func matchShell(verb, line string) []Detail {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return nil
	}
	for _, p := range shellPatterns {
		if p.re.MatchString(t) {
			if len(t) > 90 {
				t = t[:87] + "..."
			}
			return []Detail{{verb + " line", p.what + ": " + t, p.risk}}
		}
	}
	return nil
}

func stateLines(before, after string) []Detail {
	set := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				m[l] = true
			}
		}
		return m
	}
	b, a := set(before), set(after)
	var out []Detail
	for l := range a {
		if !b[l] {
			out = append(out, Detail{"appeared", l, Warn})
		}
	}
	for l := range b {
		if !a[l] {
			out = append(out, Detail{"disappeared", l, Notice})
		}
	}
	return out
}

func fileKind(e *model.Entry, body []byte, live bool) []Detail {
	var out []Detail
	kind := ""
	head := body
	if head == nil && live {
		if f, err := os.Open(e.Path); err == nil {
			buf := make([]byte, 512)
			n, _ := f.Read(buf)
			head = buf[:n]
			f.Close()
		}
	}
	switch {
	case len(head) >= 4 && (bytes.Equal(head[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}) || bytes.Equal(head[:4], []byte{0xce, 0xfa, 0xed, 0xfe}) || bytes.Equal(head[:4], []byte{0xca, 0xfe, 0xba, 0xbe})):
		kind = "Mach-O executable"
	case len(head) >= 4 && bytes.Equal(head[:4], []byte{0x7f, 'E', 'L', 'F'}):
		kind = "ELF executable"
	case bytes.HasPrefix(head, []byte("#!")):
		line := string(head)
		if i := strings.IndexByte(line, '\n'); i > 0 {
			line = line[:i]
		}
		kind = "script (" + strings.TrimSpace(line[2:]) + ")"
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		kind = "zip archive"
	}
	if kind != "" {
		out = append(out, Detail{"kind", kind, Info})
	}
	if e.Mode&0o111 != 0 {
		out = append(out, Detail{"executable", fmt.Sprintf("yes (mode %04o)", e.Mode), Notice})
	}
	if e.Mode&0o6000 != 0 {
		out = append(out, Detail{"setuid/setgid", "runs with the file owner's privileges", Warn})
	}
	if live && runtime.GOOS == "darwin" && strings.HasPrefix(kind, "Mach-O") {
		out = append(out, Detail{"signature", codesign(e.Path), Notice})
	}
	return out
}

func codesign(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "codesign", "-dv", "--verbose=2", path).CombinedOutput()
	s := string(out)
	if err != nil {
		if strings.Contains(s, "not signed") {
			return "unsigned"
		}
		return "unknown"
	}
	var auth []string
	adhoc := false
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "Authority=") && len(auth) == 0 {
			auth = append(auth, strings.TrimPrefix(l, "Authority="))
		}
		if strings.HasPrefix(l, "Signature=adhoc") {
			adhoc = true
		}
	}
	if adhoc {
		return "ad-hoc (no identity)"
	}
	if len(auth) > 0 {
		return auth[0]
	}
	return "signed"
}
