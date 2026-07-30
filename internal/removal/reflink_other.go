//go:build !linux

package removal

import (
	"errors"
	"os"
)

// cloneFile has no portable equivalent outside Linux. The deployment target is
// Linux (§17), so this exists only so the package builds and tests run on a
// developer's other machine: reflink mode reports unsupported there and the
// hardlink and trash paths still work.
func cloneFile(src, dest string, perm os.FileMode) error {
	return errors.New("reflinks are only implemented on Linux")
}
