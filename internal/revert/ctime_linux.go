package revert

import (
	"os"
	"syscall"
	"time"
)

// changed is the later of mtime and ctime. ctime cannot be set by a user
// (cp -p, touch -d preserve or forge mtime only), so a file placed into a
// tree after a commit shows up here even with an old modification time.
func changed(fi os.FileInfo) time.Time {
	t := fi.ModTime()
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if c := time.Unix(st.Ctim.Sec, st.Ctim.Nsec); c.After(t) {
			t = c
		}
	}
	return t
}
