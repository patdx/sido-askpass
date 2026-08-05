//go:build linux

package sido

import "golang.org/x/sys/unix"

func ttyFlags(fd int) (echo, icanon bool, err error) {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return false, false, err
	}
	return t.Lflag&unix.ECHO != 0, t.Lflag&unix.ICANON != 0, nil
}
