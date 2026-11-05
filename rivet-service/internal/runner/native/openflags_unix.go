//go:build unix

package native

import "syscall"

// openNonBlock — открытие без блокировки: FIFO не подвесит инструмент,
// тип файла проверяется по открытому дескриптору.
const openNonBlock = syscall.O_NONBLOCK

// openNoFollow — запись без перехода по симлинку в последнем элементе.
const openNoFollow = syscall.O_NOFOLLOW
