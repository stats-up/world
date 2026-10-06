package monitor

import "syscall"

// diskUsage devuelve lo usado y el total del sistema de archivos (como `df`, sin la reserva de root).
func diskUsage(path string) (used, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	used = int64(st.Blocks-st.Bfree) * bs
	return used, used + int64(st.Bavail)*bs, nil
}
