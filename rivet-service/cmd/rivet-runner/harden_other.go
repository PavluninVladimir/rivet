//go:build !linux

package main

// hardenProcess — на этой платформе аналога PR_SET_DUMPABLE нет: секреты
// защищает перезапуск с очищенным окружением (secrets.go).
func hardenProcess() {}

// processDumpable — неприменимо вне Linux.
func processDumpable() int { return -1 }
