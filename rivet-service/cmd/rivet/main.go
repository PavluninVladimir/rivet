// rivet — CLI для dev-операций: миграции, создание проекта/Epic, отладка.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rivet <command> (migrate, createdb, projects, epics, tasks)")
		os.Exit(2)
	}
	if err := dispatch(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "rivet:", err)
		os.Exit(1)
	}
}
