//go:build darwin

package native

import "golang.org/x/sys/unix"

// snapshotProcs на macOS берёт таблицу процессов через sysctl kern.proc.all
// (pid, родитель и время старта; окружение не читается).
var snapshotProcs = func() []procInfo {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	out := make([]procInfo, 0, len(procs))
	for _, p := range procs {
		st := p.Proc.P_starttime
		out = append(out, procInfo{
			pid:   int(p.Proc.P_pid),
			ppid:  int(p.Eproc.Ppid),
			start: uint64(st.Sec)*1_000_000 + uint64(st.Usec),
		})
	}
	return out
}
