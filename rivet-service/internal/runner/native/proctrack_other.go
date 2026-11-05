//go:build !linux && !darwin

package native

// На прочих платформах снимков процессов нет: остаётся убийство группы.
var snapshotProcs func() []procInfo
