//go:build linux

package removal

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile makes dest a copy-on-write clone of src with the FICLONE ioctl — what
// `cp --reflink` does. btrfs and XFS support it; ext4 does not, and returns
// EOPNOTSUPP, which ProbeLinkSupport turns into a readable answer.
//
// Pure syscall, no CGO (invariant 6): golang.org/x/sys/unix is a plain Go wrapper.
func cloneFile(src, dest string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()     //nolint:errcheck
		os.Remove(dest) //nolint:errcheck // leave no half-cloned file behind
		return fmt.Errorf("FICLONE %s -> %s: %w", src, dest, err)
	}
	return out.Close()
}
