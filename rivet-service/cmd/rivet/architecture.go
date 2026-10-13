package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/architecture"
)

// Команды работы с описанием архитектуры (спека backend/architecture
// «Команды работы с описанием»). Работают с локальным каталогом без
// control plane, поэтому годятся и для CI проекта.

// usageError — неверные аргументы, код выхода 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// errDefects — в описании есть ошибки, код выхода 1. Дефекты к этому
// моменту уже выведены.
var errDefects = errors.New("в описании архитектуры есть ошибки")

const architectureUsage = "usage: rivet architecture <init|validate|views|apply> [-dir " + architecture.DefaultDir + "] " +
	"[-system имя] [-change id] [-check] [-json]"

func cmdArchitecture(args []string) error {
	if len(args) == 0 {
		return usageError{architectureUsage}
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("rivet architecture "+sub, flag.ContinueOnError)
	dir := fs.String("dir", architecture.DefaultDir, "каталог описания архитектуры")
	asJSON := fs.Bool("json", false, "вывести дефекты массивом JSON")
	change := new(string)
	check := new(bool)
	system := new(string)
	switch sub {
	case "init":
		system = fs.String("system", "", "название системы, по умолчанию имя текущего каталога")
	case "validate":
		change = fs.String("change", "", "проверить изменение changes/<id> вместе с текущей моделью")
	case "views":
		check = fs.Bool("check", false, "не писать файлы, а проверить актуальность представлений")
	case "apply":
		change = fs.String("change", "", "применяемое изменение changes/<id>")
	default:
		return usageError{fmt.Sprintf("неизвестная подкоманда architecture %q\n%s", sub, architectureUsage)}
	}
	if err := fs.Parse(rest); err != nil {
		return usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usageError{"лишние аргументы: " + strings.Join(fs.Args(), " ")}
	}
	if *change != "" && !architecture.ValidChangeID(*change) {
		return usageError{fmt.Sprintf("идентификатор изменения %q: только строчные латинские буквы, цифры и дефис", *change)}
	}

	switch sub {
	case "init":
		name := *system
		if name == "" {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			name = filepath.Base(wd)
		}
		if err := architecture.Init(*dir, name); err != nil {
			return err
		}
		fmt.Fprintf(output, "описание архитектуры создано в %s\n", *dir)
		return nil
	case "validate":
		fsys, closer, err := architecture.OpenDir(*dir)
		if err != nil {
			return reportArchitecture(nil, err, *dir, *asJSON)
		}
		defer closer.Close()
		var ds architecture.Defects
		if *change != "" {
			ds, err = architecture.ValidateChange(fsys, *change)
		} else {
			ds, err = architecture.Validate(fsys)
		}
		return reportArchitecture(ds, err, *dir, *asJSON)
	case "views":
		if *check {
			fsys, closer, err := architecture.OpenDir(*dir)
			if err != nil {
				return reportArchitecture(nil, err, *dir, *asJSON)
			}
			defer closer.Close()
			ds, err := architecture.CheckViews(fsys)
			return reportArchitecture(ds, err, *dir, *asJSON)
		}
		ds, err := architecture.WriteViews(*dir)
		return reportArchitecture(ds, err, *dir, *asJSON)
	default: // apply
		if *change == "" {
			return usageError{"нужен -change <id> изменения"}
		}
		res, err := architecture.Apply(*dir, *change, time.Now())
		if err != nil {
			return err
		}
		if res.Applied && !*asJSON {
			fmt.Fprintf(output, "изменение %s применено, архив: %s\n", *change, res.Archive)
		}
		return reportArchitecture(res.Defects, nil, *dir, *asJSON)
	}
}

// reportArchitecture выводит дефекты текстом или JSON и превращает
// ошибки проверки в код выхода 1. Отсутствие описания ошибкой не
// считается: описание у проекта необязательно.
func reportArchitecture(ds architecture.Defects, err error, dir string, asJSON bool) error {
	if errors.Is(err, architecture.ErrAbsent) {
		if asJSON {
			fmt.Fprintln(output, "[]")
		} else {
			fmt.Fprintf(output, "описания архитектуры нет: %s\n", dir)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if asJSON {
		if ds == nil {
			ds = architecture.Defects{}
		}
		enc := json.NewEncoder(output)
		enc.SetIndent("", "  ")
		if err := enc.Encode(ds); err != nil {
			return err
		}
	} else {
		for _, d := range ds {
			fmt.Fprintln(output, d.String())
		}
		errs, warns := ds.Count()
		fmt.Fprintf(output, "ошибок: %d, предупреждений: %d\n", errs, warns)
	}
	if ds.HasErrors() {
		return errDefects
	}
	return nil
}
