package scan

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A collector turns a system query into stable text: anything that varies
// run to run without meaning (PIDs, ordering) is normalised away.
type collector struct {
	name string
	run  func() (string, error)
}

func collectors() []collector {
	cs := []collector{{"crontab", crontab}}
	switch runtime.GOOS {
	case "darwin":
		cs = append(cs, collector{"launchd-loaded", launchdLoaded}, collector{"listening-ports", lsofPorts})
	case "linux":
		cs = append(cs, collector{"systemd-user-units", systemdUser}, collector{"listening-ports", ssPorts})
	}
	return cs
}

func output(name string, args ...string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", errors.New(name + " not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func sortedUnique(lines []string) string {
	set := map[string]bool{}
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			set[l] = true
		}
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

func crontab() (string, error) {
	out, err := output("crontab", "-l")
	if err != nil {
		// "no crontab for user" exits non-zero: that is an empty crontab.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// launchdLoaded lists labels of jobs loaded in the user's GUI domain,
// dropping Apple's own and the per-process application.* entries.
func launchdLoaded() (string, error) {
	out, err := output("launchctl", "list")
	if err != nil {
		return "", err
	}
	var labels []string
	for i, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 3 {
			continue
		}
		l := f[2]
		if strings.HasPrefix(l, "com.apple.") || strings.HasPrefix(l, "application.") {
			continue
		}
		labels = append(labels, l)
	}
	return sortedUnique(labels), nil
}

func lsofPorts() (string, error) {
	out, err := output("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "cn")
	if err != nil && out == "" {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", nil // no listeners
		}
		return "", err
	}
	var lines []string
	cmd := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "c") {
			cmd = l[1:]
		} else if strings.HasPrefix(l, "n") {
			lines = append(lines, portOf(l[1:])+"\t"+cmd)
		}
	}
	return sortedUnique(lines), nil
}

func portOf(addr string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr
	}
	host, port := addr[:i], addr[i+1:]
	if host == "*" || host == "0.0.0.0" || host == "[::]" {
		host = "*"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return addr
	}
	return host + ":" + port
}

func systemdUser() (string, error) {
	out, err := output("systemctl", "--user", "list-unit-files", "--no-legend", "--no-pager")
	if err != nil && out == "" {
		return "", err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 {
			lines = append(lines, f[0]+"\t"+f[1])
		}
	}
	return sortedUnique(lines), nil
}

func ssPorts() (string, error) {
	out, err := output("ss", "-Hltn")
	if err != nil {
		return "", err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 {
			lines = append(lines, portOf(f[3]))
		}
	}
	return sortedUnique(lines), nil
}
