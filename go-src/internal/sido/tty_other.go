//go:build !linux && !darwin

package sido

import "errors"

func ttyFlags(fd int) (echo, icanon bool, err error) {
	return false, false, errors.New("tty flags unsupported on this platform")
}

func ttyEchoOff(fd int) (restore func(), err error) {
	return nil, errors.New("tty unsupported on this platform")
}
