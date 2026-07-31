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

// ttyEchoOff disables ECHO on the fd (keeping canonical line buffering) and
// returns a func to restore the previous termios. Used to read a hidden
// password line.
func ttyEchoOff(fd int) (restore func(), err error) {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	old := *t
	t.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return nil, err
	}
	return func() { unix.IoctlSetTermios(fd, unix.TCSETS, &old) }, nil
}
