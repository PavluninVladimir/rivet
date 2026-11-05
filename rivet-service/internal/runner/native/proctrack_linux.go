//go:build linux

package native

import (
	"os"
	"strconv"
	"strings"
)

// snapshotProcs на Linux читает /proc/<pid>/stat (только родитель и время
// старта, без окружения процессов).
var snapshotProcs = func() []procInfo {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]procInfo, 0, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if p, ok := readStat(pid); ok {
			out = append(out, p)
		}
	}
	return out
}

// readStat разбирает /proc/<pid>/stat: имя процесса в скобках может
// содержать пробелы, поэтому поля считаются от последней «)».
func readStat(pid int) (procInfo, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procInfo{}, false
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procInfo{}, false
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] — состояние (поле 3), ppid — поле 4, starttime — поле 22.
	if len(fields) < 20 {
		return procInfo{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return procInfo{}, false
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procInfo{}, false
	}
	return procInfo{pid: pid, ppid: ppid, start: start}, true
}
