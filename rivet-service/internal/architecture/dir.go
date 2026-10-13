package architecture

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// OpenDir открывает каталог описания для чтения. Файлы читаются только
// внутри каталога: симлинк наружу даёт ошибку, а не чужой файл. Это важно
// runner'у, который отправляет описание из клона в control plane.
// Несуществующий каталог означает, что описания нет.
func OpenDir(dir string) (fs.FS, io.Closer, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, ErrAbsent
	}
	if err != nil {
		return nil, nil, err
	}
	return root.FS(), root, nil
}

// writeFile пишет файл внутри корня, создавая каталоги. Запись через
// симлинк запрещена в любом компоненте пути: даже симлинк внутри
// описания (views/x.md → model/x.yaml) позволил бы генератору затереть
// модель.
func writeFile(root *os.Root, name string, data []byte) error {
	parts := strings.Split(name, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		fi, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: запись через симлинк запрещена", p)
		}
	}
	if dir := parentDir(name); dir != "" {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return root.WriteFile(name, data, 0o644)
}

func parentDir(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			return name[:i]
		}
	}
	return ""
}
