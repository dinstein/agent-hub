//go:build !darwin && !linux && !windows

package calllog

import "errors"

func freeBytes(string) (int64, error) {
	return 0, errors.New("free-space inspection is unsupported on this platform")
}
