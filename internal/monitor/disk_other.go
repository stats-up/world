//go:build !linux

package monitor

import "errors"

func diskUsage(string) (int64, int64, error) {
	return 0, 0, errors.New("el monitoreo del disco solo funciona en Linux")
}
