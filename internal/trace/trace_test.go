package trace

import (
	"strings"
	"testing"
)

// Shapes follow eslogger's JSON (es_message_t field names).
const esFixture = `{"event_type":11,"process":{"audit_token":{"pid":100},"ppid":1,"executable":{"path":"/bin/zsh"}},"event":{"fork":{"child":{"audit_token":{"pid":101},"executable":{"path":"/bin/zsh"}}}}}
{"event_type":9,"process":{"audit_token":{"pid":101},"ppid":100,"executable":{"path":"/bin/zsh"}},"event":{"exec":{"target":{"audit_token":{"pid":101},"executable":{"path":"/bin/sh"}},"args":["sh","postinstall"]}}}
{"event_type":13,"process":{"audit_token":{"pid":101},"ppid":100,"executable":{"path":"/bin/sh"}},"event":{"create":{"destination_type":1,"destination":{"new_path":{"dir":{"path":"/Users/u/Library/LaunchAgents"},"filename":"com.foo.plist","mode":420}}}}}
{"event_type":14,"process":{"audit_token":{"pid":102},"ppid":101,"executable":{"path":"/usr/bin/tee"}},"event":{"close":{"modified":true,"target":{"path":"/Users/u/.zshrc"}}}}
{"event_type":14,"process":{"audit_token":{"pid":555},"ppid":1,"executable":{"path":"/usr/libexec/mds"}},"event":{"close":{"modified":true,"target":{"path":"/Users/u/unrelated"}}}}
{"event_type":25,"process":{"audit_token":{"pid":101},"ppid":100,"executable":{"path":"/bin/sh"}},"event":{"rename":{"source":{"path":"/tmp/x"},"destination_type":0,"destination":{"existing_file":{"path":"/usr/local/bin/foo"}}}}}
{"event_type":32,"process":{"audit_token":{"pid":101},"ppid":100,"executable":{"path":"/bin/sh"}},"event":{"unlink":{"target":{"path":"/Users/u/.oldrc"},"parent_dir":{"path":"/Users/u"}}}}
not json at all`

func TestParseESLogger(t *testing.T) {
	w := ParseESLogger(strings.NewReader(esFixture), 100)
	if got := w["/Users/u/Library/LaunchAgents/com.foo.plist"]; len(got) != 1 || got[0].Exe != "/bin/sh" || got[0].Op != "create" {
		t.Errorf("create: %+v", got)
	}
	if got := w["/Users/u/.zshrc"]; len(got) != 1 || got[0].Pid != 102 {
		t.Errorf("grandchild write via ppid: %+v", got)
	}
	if _, ok := w["/Users/u/unrelated"]; ok {
		t.Error("process outside the tree was attributed")
	}
	if got := w["/usr/local/bin/foo"]; len(got) != 1 || got[0].Op != "rename-to" {
		t.Errorf("rename: %+v", got)
	}
	if got := w["/Users/u/.oldrc"]; len(got) != 1 || got[0].Op != "unlink" {
		t.Errorf("unlink: %+v", got)
	}
}

const straceFixture = `4000 execve("/bin/sh", ["sh", "install.sh"], 0x7ffd /* 20 vars */) = 0
4000 openat(AT_FDCWD, "/etc/ld.so.cache", O_RDONLY|O_CLOEXEC) = 3
4000 chdir("/home/u") = 0
4000 clone(child_stack=NULL, flags=CLONE_CHILD_CLEARTID|SIGCHLD) = 4001
4001 execve("/usr/bin/mkdir", ["mkdir", "-p", ".foo/bin"], 0x5 /* 20 vars */) = 0
4001 mkdir(".foo", 0777)             = 0
4001 mkdir(".foo/bin", 0777 <unfinished ...>
4000 openat(AT_FDCWD, "/home/u/.bashrc", O_WRONLY|O_CREAT|O_APPEND, 0666 <unfinished ...>
4001 <... mkdir resumed>)            = 0
4000 <... openat resumed>)           = 4
4000 unlink("/home/u/missing")       = -1 ENOENT (No such file or directory)
4000 renameat2(AT_FDCWD, "/tmp/dl", AT_FDCWD, "/home/u/.foo/bin/foo", RENAME_NOREPLACE) = 0
4000 +++ exited with 0 +++`

func TestParseStrace(t *testing.T) {
	w := ParseStrace(strings.NewReader(straceFixture), "/tmp/start")
	if _, ok := w["/etc/ld.so.cache"]; ok {
		t.Error("read-only open attributed")
	}
	if got := w["/home/u/.foo/bin"]; len(got) != 1 || got[0].Pid != 4001 || got[0].Exe != "/usr/bin/mkdir" {
		t.Errorf("relative mkdir after chdir+clone, resumed call: %+v", got)
	}
	if got := w["/home/u/.bashrc"]; len(got) != 1 || got[0].Exe != "/bin/sh" {
		t.Errorf("resumed openat: %+v", got)
	}
	if _, ok := w["/home/u/missing"]; ok {
		t.Error("failed syscall attributed")
	}
	if got := w["/home/u/.foo/bin/foo"]; len(got) != 1 || got[0].Op != "rename-to" {
		t.Errorf("renameat2: %+v", got)
	}
}
