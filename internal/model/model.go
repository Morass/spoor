// Package model holds the data types shared by every spoor package:
// manifests (what the watched parts of a machine looked like), commits
// (a recorded step in that machine's history) and changes between them.
package model

import "time"

type EntryType string

const (
	File    EntryType = "file"
	Dir     EntryType = "dir"
	Symlink EntryType = "symlink"
	Other   EntryType = "other"
	// State is a virtual entry: normalised output of a system query
	// (loaded launchd jobs, crontab, listening ports) stored as text.
	State EntryType = "state"
)

// StatePrefix marks virtual paths produced by state collectors.
const StatePrefix = "@state/"

type Entry struct {
	Path  string    `json:"p"`
	Type  EntryType `json:"t"`
	Mode  uint32    `json:"m,omitempty"`
	Size  int64     `json:"s,omitempty"`
	MTime int64     `json:"mt,omitempty"`
	// Hash is the sha256 of the content when it was read.
	Hash string `json:"h,omitempty"`
	// Stored is true when the content is kept in the object store,
	// so it can be diffed and restored.
	Stored bool   `json:"st,omitempty"`
	Link   string `json:"l,omitempty"`
	// Skipped says why content was not stored: size, sensitive,
	// unreadable, metadata (root is metadata-only).
	Skipped string `json:"sk,omitempty"`
}

// Root is one watched location.
type Root struct {
	Path string `json:"path"`
	// Depth limits recursion: 0 = the root only, -1 = unlimited.
	Depth int `json:"depth"`
	// Content means file bodies are captured (subject to size and
	// sensitivity rules); otherwise only metadata is recorded.
	Content bool `json:"content"`
}

type Manifest struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Host    string    `json:"host"`
	OS      string    `json:"os"`
	Home    string    `json:"home"`
	Roots   []Root    `json:"roots"`
	State   bool      `json:"state"`
	Entries []Entry   `json:"entries"`
}

type CommitKind string

const (
	KindSnap   CommitKind = "snap"
	KindDrift  CommitKind = "drift"
	KindRun    CommitKind = "run"
	KindRevert CommitKind = "revert"
	KindTry    CommitKind = "try"
)

type Commit struct {
	ID       string     `json:"id"`
	Parent   string     `json:"parent,omitempty"`
	Kind     CommitKind `json:"kind"`
	Time     time.Time  `json:"time"`
	Message  string     `json:"message,omitempty"`
	Command  []string   `json:"command,omitempty"`
	Cwd      string     `json:"cwd,omitempty"`
	ExitCode int        `json:"exit_code"`
	Duration float64    `json:"duration_s,omitempty"`
	Pre      string     `json:"pre,omitempty"`
	Post     string     `json:"post"`
	Traced   bool       `json:"traced,omitempty"`
	// TryState is pending|applied|discarded for KindTry commits, which
	// never move HEAD; Workspace is where the overlay upper dirs live.
	TryState  string `json:"try_state,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Reverts   string `json:"reverts,omitempty"`
}

type ChangeKind string

const (
	Added      ChangeKind = "added"
	Removed    ChangeKind = "removed"
	Modified   ChangeKind = "modified"
	Meta       ChangeKind = "meta"
	Renamed    ChangeKind = "renamed"
	TypeChange ChangeKind = "typechange"
)

type Change struct {
	Path    string     `json:"path"`
	OldPath string     `json:"old_path,omitempty"`
	Kind    ChangeKind `json:"kind"`
	Before  *Entry     `json:"before,omitempty"`
	After   *Entry     `json:"after,omitempty"`
}

// Symbol is the one-character marker used in lists.
func (k ChangeKind) Symbol() string {
	switch k {
	case Added:
		return "+"
	case Removed:
		return "-"
	case Modified:
		return "~"
	case Meta:
		return "m"
	case Renamed:
		return ">"
	case TypeChange:
		return "!"
	}
	return "?"
}

// Writer is a process observed writing a path during a traced run.
type Writer struct {
	Pid int    `json:"pid"`
	Exe string `json:"exe"`
	Op  string `json:"op"`
}
