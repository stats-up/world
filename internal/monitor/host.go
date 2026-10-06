package monitor

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

// cpuTimes son los contadores acumulados de la línea "cpu" de /proc/stat.
type cpuTimes struct{ busy, total uint64 }

func parseProcStat(data string) (cpuTimes, error) {
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t cpuTimes
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			if i == 8 || i == 9 { // guest y guest_nice ya están incluidos en user y nice
				continue
			}
			t.total += n
			if i != 3 && i != 4 { // idle e iowait
				t.busy += n
			}
		}
		return t, nil
	}
	return cpuTimes{}, errors.New("/proc/stat sin línea cpu")
}

// parseMeminfo devuelve los valores de /proc/meminfo en bytes.
func parseMeminfo(data string) map[string]int64 {
	out := map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, _ := strconv.ParseInt(f[0], 10, 64)
		if len(f) > 1 && f[1] == "kB" {
			n *= 1024
		}
		out[k] = n
	}
	return out
}

// hostRaw es una lectura del servidor sin la CPU calculada (se necesita la anterior para el delta).
type hostRaw struct {
	cpu                 cpuTimes
	memUsed, memTotal   int64
	swapUsed, swapTotal int64
	diskUsed, diskTotal int64
	load1               float64
}

func readHost(proc, diskPath string) (hostRaw, error) {
	var h hostRaw
	stat, err := os.ReadFile(proc + "/stat")
	if err != nil {
		return h, err
	}
	if h.cpu, err = parseProcStat(string(stat)); err != nil {
		return h, err
	}
	mi, err := os.ReadFile(proc + "/meminfo")
	if err != nil {
		return h, err
	}
	m := parseMeminfo(string(mi))
	h.memTotal = m["MemTotal"]
	h.memUsed = m["MemTotal"] - m["MemAvailable"]
	h.swapTotal = m["SwapTotal"]
	h.swapUsed = m["SwapTotal"] - m["SwapFree"]
	if la, err := os.ReadFile(proc + "/loadavg"); err == nil {
		if f := strings.Fields(string(la)); len(f) > 0 {
			h.load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	h.diskUsed, h.diskTotal, err = diskUsage(diskPath)
	return h, err
}
