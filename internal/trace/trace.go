// Package trace attributes file changes to the processes that made them:
// eslogger (Endpoint Security) on macOS, strace on Linux.
package trace

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/morass/spoor/internal/model"
)

type Session struct {
	kind   string
	out    string
	cmd    *exec.Cmd
	root   int
	cwd    string
	argv   []string
	stderr strings.Builder
}

// Prepare readies tracing for argv. On Linux it returns argv wrapped in
// strace; on macOS it starts eslogger (needs root) and returns argv as is.
func Prepare(argv []string, cwd, tmpDir string) (*Session, []string, error) {
	out := filepath.Join(tmpDir, "trace-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	s := &Session{out: out, cwd: cwd}
	switch runtime.GOOS {
	case "linux":
		if _, err := exec.LookPath("strace"); err != nil {
			return nil, nil, errors.New("--trace needs strace on Linux (apt install strace)")
		}
		s.kind = "strace"
		wrapped := append([]string{"strace", "-f", "-qq", "-s", "4096", "-o", out,
			"-e", "trace=open,openat,creat,rename,renameat,renameat2,unlink,unlinkat,rmdir,mkdir,mkdirat,symlink,symlinkat,link,linkat,chmod,fchmodat,execve,chdir,clone,clone3,fork,vfork",
			"--"}, argv...)
		return s, wrapped, nil
	case "darwin":
		if _, err := exec.LookPath("eslogger"); err != nil {
			return nil, nil, errors.New("--trace needs eslogger (macOS 13+)")
		}
		args := []string{"eslogger", "create", "rename", "unlink", "close", "exec", "fork"}
		if os.Geteuid() != 0 {
			if exec.Command("sudo", "-n", "true").Run() != nil {
				return nil, nil, errors.New("--trace on macOS needs root for eslogger: run `sudo -v` first (or run spoor with sudo)")
			}
			args = append([]string{"sudo", "-n"}, args...)
		}
		f, err := os.Create(out)
		if err != nil {
			return nil, nil, err
		}
		s.kind = "eslogger"
		s.cmd = exec.Command(args[0], args[1:]...)
		s.cmd.Stdout = f
		s.cmd.Stderr = &s.stderr
		s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := s.cmd.Start(); err != nil {
			f.Close()
			return nil, nil, err
		}
		time.Sleep(700 * time.Millisecond) // let the ES client subscribe
		if s.cmd.ProcessState != nil {
			return nil, nil, fmt.Errorf("eslogger exited: %s", s.stderr.String())
		}
		return s, argv, nil
	}
	return nil, nil, fmt.Errorf("--trace is not supported on %s", runtime.GOOS)
}

// SetRoot tells the session which pid is the traced command.
func (s *Session) SetRoot(pid int) { s.root = pid }

// Finish stops the tracer and returns path → writers.
func (s *Session) Finish() (map[string][]model.Writer, error) {
	defer os.Remove(s.out)
	switch s.kind {
	case "strace":
		f, err := os.Open(s.out)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return ParseStrace(f, s.cwd), nil
	case "eslogger":
		time.Sleep(500 * time.Millisecond) // drain in-flight events
		if s.cmd.Process != nil {
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGINT)
			done := make(chan struct{})
			go func() { s.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
				<-done
			}
		}
		f, err := os.Open(s.out)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return ParseESLogger(f, s.root), nil
	}
	return nil, nil
}

func add(m map[string][]model.Writer, path string, w model.Writer) {
	if path == "" {
		return
	}
	path = filepath.Clean(path)
	for _, e := range m[path] {
		if e == w {
			return
		}
	}
	if len(m[path]) < 8 {
		m[path] = append(m[path], w)
	}
}

// ---- eslogger ----

func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func intOf(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// destination handles es_destination: an existing file or dir+filename.
func destination(d any) string {
	if p := str(dig(d, "existing_file", "path")); p != "" {
		return p
	}
	dir := str(dig(d, "new_path", "dir", "path"))
	name := str(dig(d, "new_path", "filename"))
	if dir != "" && name != "" {
		return filepath.Join(dir, name)
	}
	return ""
}

// ParseESLogger reads eslogger NDJSON and keeps events from root and its
// descendants.
func ParseESLogger(r io.Reader, root int) map[string][]model.Writer {
	out := map[string][]model.Writer{}
	tree := map[int]bool{root: true}
	exe := map[int]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var msg map[string]any
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		proc := msg["process"]
		pid := intOf(dig(proc, "audit_token", "pid"))
		ppid := intOf(dig(proc, "ppid"))
		if !tree[pid] && root != 0 {
			if tree[ppid] {
				tree[pid] = true
			} else {
				continue
			}
		}
		if exe[pid] == "" {
			exe[pid] = str(dig(proc, "executable", "path"))
		}
		ev, _ := msg["event"].(map[string]any)
		w := func(op string) model.Writer { return model.Writer{Pid: pid, Exe: exe[pid], Op: op} }
		for name, body := range ev {
			switch name {
			case "fork":
				if c := intOf(dig(body, "child", "audit_token", "pid")); c != 0 {
					tree[c] = true
					exe[c] = exe[pid]
				}
			case "exec":
				if p := str(dig(body, "target", "executable", "path")); p != "" {
					exe[pid] = p
				}
			case "create":
				add(out, destination(dig(body, "destination")), w("create"))
			case "rename":
				add(out, str(dig(body, "source", "path")), w("rename-from"))
				add(out, destination(dig(body, "destination")), w("rename-to"))
			case "unlink":
				add(out, str(dig(body, "target", "path")), w("unlink"))
			case "close":
				if m, _ := dig(body, "modified").(bool); m {
					add(out, str(dig(body, "target", "path")), w("write"))
				}
			}
		}
	}
	return out
}

// ---- strace ----

var (
	straceLine    = regexp.MustCompile(`^(\d+)\s+(.*)$`)
	straceCall    = regexp.MustCompile(`^(\w+)\((.*)\)\s+=\s+(-?\d+|\?)`)
	unfinished    = regexp.MustCompile(`^(\w+)\((.*) <unfinished \.\.\.>$`)
	resumed       = regexp.MustCompile(`^<\.\.\. (\w+) resumed>(.*)\)\s+=\s+(-?\d+|\?)`)
	quoted        = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	writeFlags    = regexp.MustCompile(`O_WRONLY|O_RDWR|O_CREAT|O_TRUNC`)
)

func unquote(s string) string {
	if u, err := strconv.Unquote(`"` + s + `"`); err == nil {
		return u
	}
	return s
}

// ParseStrace reads `strace -f -o` output. cwd is the command's start dir.
func ParseStrace(r io.Reader, cwd string) map[string][]model.Writer {
	out := map[string][]model.Writer{}
	exe := map[int]string{}
	dirs := map[int]string{}
	pending := map[int]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	first := 0
	for sc.Scan() {
		m := straceLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		if first == 0 {
			first = pid
			dirs[pid] = cwd
		}
		rest := m[2]
		var name, args, ret string
		if u := unfinished.FindStringSubmatch(rest); u != nil {
			pending[pid] = u[1] + "\x00" + u[2]
			continue
		} else if rs := resumed.FindStringSubmatch(rest); rs != nil {
			p := strings.SplitN(pending[pid], "\x00", 2)
			delete(pending, pid)
			if len(p) != 2 {
				continue
			}
			name, args, ret = p[0], p[1]+rs[2], rs[3]
		} else if c := straceCall.FindStringSubmatch(rest); c != nil {
			name, args, ret = c[1], c[2], c[3]
		} else {
			continue
		}
		if ret == "?" || strings.HasPrefix(ret, "-") {
			continue
		}
		dir := dirs[pid]
		if dir == "" {
			dir = cwd
		}
		qs := quoted.FindAllStringSubmatch(args, -1)
		path := func(i int) string {
			if i < 0 {
				i = len(qs) + i
			}
			if i < 0 || i >= len(qs) {
				return ""
			}
			p := unquote(qs[i][1])
			if !filepath.IsAbs(p) {
				if strings.HasPrefix(args, "AT_FDCWD") || !strings.HasSuffix(name, "at") || strings.HasPrefix(name, "rename") || strings.HasPrefix(name, "link") || strings.HasPrefix(name, "symlink") {
					p = filepath.Join(dir, p)
				} else {
					return ""
				}
			}
			return p
		}
		w := func(op string) model.Writer { return model.Writer{Pid: pid, Exe: exe[pid], Op: op} }
		switch name {
		case "clone", "clone3", "fork", "vfork":
			if child, err := strconv.Atoi(ret); err == nil {
				dirs[child] = dir
				exe[child] = exe[pid]
			}
		case "execve":
			exe[pid] = path(0)
		case "chdir":
			dirs[pid] = path(0)
		case "open", "openat", "creat":
			if name == "creat" || writeFlags.MatchString(args) {
				add(out, path(0), w("write"))
			}
		case "rename", "renameat", "renameat2":
			add(out, path(0), w("rename-from"))
			add(out, path(1), w("rename-to"))
		case "unlink", "unlinkat", "rmdir":
			add(out, path(0), w("unlink"))
		case "mkdir", "mkdirat":
			add(out, path(0), w("mkdir"))
		case "symlink", "symlinkat", "link", "linkat":
			add(out, path(-1), w("link"))
		case "chmod", "fchmodat":
			add(out, path(0), w("chmod"))
		}
	}
	return out
}
