//go:build !darwin

package store

func cloneOrCopy(src, dst string) error {
	return copyFile(src, dst)
}
