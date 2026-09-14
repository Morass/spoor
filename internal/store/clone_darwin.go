package store

import "golang.org/x/sys/unix"

// cloneOrCopy uses clonefile(2): on APFS the copy shares blocks with the
// original, so capturing even large files costs no time or space.
func cloneOrCopy(src, dst string) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return nil
	}
	return copyFile(src, dst)
}
