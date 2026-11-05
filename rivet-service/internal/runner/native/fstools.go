package native

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"

	"encoding/json"
)

// Файловые инструменты (design, решение 4). Весь ввод-вывод идёт через
// os.Root рабочей копии: выйти за корень нельзя ни путём, ни симлинком,
// в том числе подменённым между проверкой и обращением. Читаются только
// обычные файлы: FIFO, устройства и сокеты отклоняются до чтения, файл
// открывается без блокировки и проверяется уже по открытому дескриптору.
// Большие файлы читаются потоково до лимитов, а не целиком в память.

const (
	readMaxLines     = 2000
	readMaxBytes     = 64 << 10
	readLineClip     = 4 << 10  // длинная строка показывается началом с пометкой
	readCountLimit   = 16 << 20 // до этого размера считается общее число строк
	listMaxEntries   = 1000
	searchMaxMatches = 200
	searchMaxFile    = 2 << 20
	searchMaxFiles   = 5000
	searchMaxBytes   = 64 << 20
	searchLineClip   = 300
	writeMaxBytes    = 1 << 20
	binarySniff      = 8000
)

// skipDirs — каталоги зависимостей, окружений и кешей, которые search не
// обходит (если поиск не начат внутри них явно).
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, ".tox": true, ".mypy_cache": true, ".pytest_cache": true,
	".next": true, ".nuxt": true, ".gradle": true, ".cache": true,
}

// fsTools — файловые инструменты одной сессии.
type fsTools struct {
	ws *workspace
}

func isBinary(b []byte) bool {
	if len(b) > binarySniff {
		b = b[:binarySniff]
	}
	return bytes.IndexByte(b, 0) >= 0
}

func errNotExist(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("не существует")
	}
	return err
}

// openRegular открывает обычный файл рабочей копии на чтение. Проверка
// типа делается по открытому дескриптору: файл, подменённый FIFO между
// Stat и Open, не заблокирует инструмент (открытие без блокировки).
func (t *fsTools) openRegular(rel string) (*os.File, os.FileInfo, error) {
	st, err := t.ws.r.Stat(rel)
	if err != nil {
		return nil, nil, fmt.Errorf("файл %s: %w", rel, errNotExist(err))
	}
	if st.IsDir() {
		return nil, nil, fmt.Errorf("%s — каталог, используй list_dir", rel)
	}
	if !st.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s — не обычный файл (%s)", rel, st.Mode().Type())
	}
	f, err := t.ws.r.OpenFile(rel, os.O_RDONLY|openNonBlock, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("файл %s: %w", rel, errNotExist(err))
	}
	st, err = f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s — не обычный файл (%s)", rel, st.Mode().Type())
	}
	return f, st, nil
}

// lineReader — построчное чтение с ограниченным буфером: строка длиннее
// предела отдаётся началом, остаток пропускается с подсчётом байт.
type lineReader struct {
	br *bufio.Reader
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, readLineClip)}
}

// next — очередная строка без перевода строки, её полный размер в байтах,
// признак обрезки (строка длиннее readLineClip) и конца файла (строка при
// eof может быть непустой).
func (lr *lineReader) next() (line string, size int, clipped, eof bool, err error) {
	var head []byte
	for {
		chunk, e := lr.br.ReadSlice('\n')
		size += len(chunk)
		if room := readLineClip - len(head); room > 0 {
			head = append(head, chunk[:min(room, len(chunk))]...)
		}
		switch {
		case errors.Is(e, bufio.ErrBufferFull):
			continue
		case e == nil:
			body := size - 1
			head = bytes.TrimSuffix(head, []byte("\n"))
			clipped = body > len(head)
		default:
			clipped = size > len(head)
			eof = true
			if !errors.Is(e, io.EOF) {
				err = e
			}
		}
		line = string(bytes.TrimSuffix(head, []byte("\r")))
		if clipped {
			line = strings.ToValidUTF8(line, "")
		}
		return line, size, clipped, eof, err
	}
}

func (t *fsTools) readFile(ctx context.Context, raw json.RawMessage) toolOutput {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return errOut("", err)
	}
	rel, err := t.ws.resolve(a.Path, false)
	if err != nil {
		return errOut(a.Path, err)
	}
	f, st, err := t.openRegular(rel)
	if err != nil {
		return errOut(rel, err)
	}
	defer func() { _ = f.Close() }()
	if st.Size() == 0 {
		return toolOutput{text: "(файл пуст)", detail: rel}
	}
	sniff := make([]byte, binarySniff)
	n, _ := io.ReadFull(f, sniff)
	if isBinary(sniff[:n]) {
		return errOut(rel, fmt.Errorf("%s — двоичный файл, %d байт", rel, st.Size()))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return errOut(rel, err)
	}
	offset := max(a.Offset, 1)
	limit := a.Limit
	if limit <= 0 || limit > readMaxLines {
		limit = readMaxLines
	}
	countAll := st.Size() <= readCountLimit
	lr := newLineReader(f)
	var b strings.Builder
	lineNo, last, total := 0, offset-1, -1
	full := false // вывод упёрся в лимит строк или байт
	for {
		if lineNo%1000 == 0 && ctx.Err() != nil {
			return errOut(rel, ctx.Err())
		}
		line, size, clipped, eof, err := lr.next()
		if err != nil {
			return errOut(rel, err)
		}
		if eof && size == 0 {
			break
		}
		lineNo++
		if lineNo >= offset && !full {
			text := line
			if clipped {
				text += fmt.Sprintf(" … [строка обрезана: %d байт]", size)
			}
			out := fmt.Sprintf("%6d\t%s\n", lineNo, text)
			if lineNo >= offset+limit || b.Len()+len(out) > readMaxBytes {
				full = true
			} else {
				b.WriteString(out)
				last = lineNo
			}
		}
		if full && !countAll {
			break
		}
		if eof {
			break
		}
	}
	if !full || countAll {
		total = lineNo
	}
	if offset > lineNo && !full {
		return errOut(rel, fmt.Errorf("в файле %d строк, offset=%d за концом", lineNo, offset))
	}
	if full {
		if total >= 0 {
			fmt.Fprintf(&b, "… [показаны строки %d–%d из %d, продолжи с offset=%d]\n", offset, last, total, last+1)
		} else {
			fmt.Fprintf(&b, "… [показаны строки %d–%d, файл %d байт, продолжи с offset=%d]\n", offset, last, st.Size(), last+1)
		}
	}
	return toolOutput{text: b.String(), detail: rel}
}

// skipGit — каталог .git (в любом регистре) не показывается и не обходится.
func skipGit(name string) bool { return strings.EqualFold(name, ".git") }

func (t *fsTools) listDir(ctx context.Context, raw json.RawMessage) toolOutput {
	var a struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return errOut("", err)
	}
	depth := min(max(a.Depth, 1), 3)
	rel, err := t.ws.resolve(a.Path, false)
	if err != nil {
		return errOut(a.Path, err)
	}
	st, err := t.ws.r.Stat(rel)
	if err != nil {
		return errOut(rel, fmt.Errorf("каталог %s: %w", rel, errNotExist(err)))
	}
	if !st.IsDir() {
		return errOut(rel, fmt.Errorf("%s — файл, используй read_file", rel))
	}
	var entries []string
	truncated := false
	_ = fs.WalkDir(t.ws.r.FS(), rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == rel {
			return nil
		}
		if ctx.Err() != nil {
			return fs.SkipAll
		}
		r := strings.TrimPrefix(p, rel+"/")
		if rel == "." {
			r = p
		}
		level := strings.Count(r, "/") + 1
		if d.IsDir() && skipGit(d.Name()) {
			return fs.SkipDir
		}
		if len(entries) >= listMaxEntries {
			truncated = true
			return fs.SkipAll
		}
		name := r
		switch {
		case d.IsDir():
			name += "/"
		case d.Type()&fs.ModeSymlink != 0:
			name += "@"
		}
		entries = append(entries, name)
		if d.IsDir() && level >= depth {
			return fs.SkipDir
		}
		return nil
	})
	if ctx.Err() != nil {
		return errOut(rel, ctx.Err())
	}
	if len(entries) == 0 {
		return toolOutput{text: "(каталог пуст)", detail: rel}
	}
	text := strings.Join(entries, "\n")
	if truncated {
		text += fmt.Sprintf("\n… [показаны первые %d записей]", listMaxEntries)
	}
	return toolOutput{text: text, detail: rel}
}

func (t *fsTools) search(ctx context.Context, raw json.RawMessage) toolOutput {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return errOut("", err)
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return errOut(a.Pattern, fmt.Errorf("регулярное выражение: %v", err))
	}
	start, err := t.ws.resolve(a.Path, false)
	if err != nil {
		return errOut(a.Path, err)
	}
	var out []string
	files, scanned := 0, int64(0)
	stopped := "" // причина остановки обхода
	_ = fs.WalkDir(t.ws.r.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			stopped = "отмена"
			return fs.SkipAll
		}
		if d.IsDir() {
			if p != start && (skipGit(d.Name()) || skipDirs[d.Name()]) {
				return fs.SkipDir
			}
			return nil
		}
		// Только обычные файлы: симлинки, FIFO и устройства не читаются.
		if !d.Type().IsRegular() {
			return nil
		}
		if a.Glob != "" && !globMatch(a.Glob, p) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > searchMaxFile {
			return nil
		}
		if files >= searchMaxFiles {
			stopped = fmt.Sprintf("просмотрено %d файлов", files)
			return fs.SkipAll
		}
		if scanned+info.Size() > searchMaxBytes {
			stopped = fmt.Sprintf("прочитано %d МиБ", scanned>>20)
			return fs.SkipAll
		}
		files++
		scanned += info.Size()
		f, _, err := t.openRegular(p)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		sniff := make([]byte, binarySniff)
		n, _ := io.ReadFull(f, sniff)
		if isBinary(sniff[:n]) {
			return nil
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil
		}
		lr := newLineReader(io.LimitReader(f, searchMaxFile))
		for i := 1; ; i++ {
			line, size, _, eof, err := lr.next()
			if err != nil || (eof && size == 0) {
				break
			}
			if re.MatchString(line) {
				if len(out) >= searchMaxMatches {
					stopped = fmt.Sprintf("показаны первые %d совпадений", searchMaxMatches)
					return fs.SkipAll
				}
				out = append(out, fmt.Sprintf("%s:%d: %s", p, i, clip(line, searchLineClip)))
			}
			if eof {
				break
			}
		}
		return nil
	})
	if ctx.Err() != nil {
		return errOut(a.Pattern, ctx.Err())
	}
	if len(out) == 0 {
		text := "совпадений нет"
		if stopped != "" {
			text += fmt.Sprintf(" (поиск остановлен: %s, сузь path или glob)", stopped)
		}
		return toolOutput{text: text, detail: a.Pattern}
	}
	text := strings.Join(out, "\n")
	if stopped != "" {
		text += fmt.Sprintf("\n… [поиск остановлен: %s, сузь поиск]", stopped)
	}
	return toolOutput{text: text, detail: a.Pattern}
}

// globMatch — шаблон с «/» сверяется с путём от корня, без — с именем файла.
func globMatch(glob, rel string) bool {
	if strings.Contains(glob, "/") {
		ok, _ := path.Match(glob, rel)
		return ok
	}
	ok, _ := path.Match(glob, path.Base(rel))
	return ok
}

// writeTarget — права для записи: у существующего обычного файла его
// права, у нового 0644; каталог и не обычный файл — ошибка.
func (t *fsTools) writeTarget(rel string) (os.FileMode, error) {
	st, err := t.ws.r.Lstat(rel)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return 0o644, nil
	case err != nil:
		return 0, err
	case st.IsDir():
		return 0, fmt.Errorf("%s — каталог", rel)
	case !st.Mode().IsRegular():
		return 0, fmt.Errorf("%s — не обычный файл (%s)", rel, st.Mode().Type())
	}
	return st.Mode().Perm(), nil
}

// writeRegular записывает файл через os.Root без перехода по симлинку в
// последнем элементе пути (openNoFollow): подменённый между проверкой и
// записью путь даёт ошибку, а не запись по ссылке.
func (t *fsTools) writeRegular(rel string, data []byte, mode os.FileMode) error {
	f, err := t.ws.r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|openNoFollow, mode)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = fmt.Errorf("%s — не обычный файл", rel)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (t *fsTools) writeFile(_ context.Context, raw json.RawMessage) toolOutput {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return errOut("", err)
	}
	if len(a.Content) > writeMaxBytes {
		return errOut(a.Path, fmt.Errorf("содержимое %d байт больше предела %d", len(a.Content), writeMaxBytes))
	}
	rel, err := t.ws.resolve(a.Path, true)
	if err != nil {
		return errOut(a.Path, err)
	}
	if rel == "." {
		return errOut(rel, errors.New("нужен путь файла"))
	}
	mode, err := t.writeTarget(rel)
	if err != nil {
		return errOut(rel, err)
	}
	if dir := path.Dir(rel); dir != "." {
		if err := t.ws.r.MkdirAll(dir, 0o755); err != nil {
			return errOut(rel, err)
		}
	}
	// Перепроверка после создания каталогов: путь мог стать путём в .git
	// или через симлинк только за счёт состояния, созданного командой run.
	if _, err := t.ws.resolve(a.Path, true); err != nil {
		return errOut(a.Path, err)
	}
	if err := t.writeRegular(rel, []byte(a.Content), mode); err != nil {
		return errOut(rel, err)
	}
	return toolOutput{text: fmt.Sprintf("записан %s (%d байт)", rel, len(a.Content)), detail: rel, files: []string{rel}}
}

func (t *fsTools) editFile(_ context.Context, raw json.RawMessage) toolOutput {
	var a struct {
		Path       string `json:"path"`
		Old        string `json:"old"`
		New        string `json:"new"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return errOut("", err)
	}
	if a.Old == "" {
		return errOut(a.Path, errors.New("фрагмент old пуст: для нового файла используй write_file"))
	}
	rel, err := t.ws.resolve(a.Path, true)
	if err != nil {
		return errOut(a.Path, err)
	}
	f, st, err := t.openRegular(rel)
	if err != nil {
		return errOut(rel, err)
	}
	if st.Size() > writeMaxBytes {
		_ = f.Close()
		return errOut(rel, fmt.Errorf("файл %d байт больше предела правки %d: правь его командой run или разбей файл", st.Size(), writeMaxBytes))
	}
	data, err := io.ReadAll(io.LimitReader(f, writeMaxBytes+1))
	_ = f.Close()
	if err != nil {
		return errOut(rel, err)
	}
	if len(data) > writeMaxBytes {
		return errOut(rel, fmt.Errorf("файл больше предела правки %d байт", writeMaxBytes))
	}
	content := string(data)
	n := strings.Count(content, a.Old)
	switch {
	case n == 0:
		return errOut(rel, fmt.Errorf("фрагмент не найден в %s: прочитай файл и повтори с точным текстом", rel))
	case n > 1 && !a.ReplaceAll:
		return errOut(rel, fmt.Errorf("фрагмент встречается в %s %d раз: уточни фрагмент или укажи replace_all", rel, n))
	}
	if a.ReplaceAll {
		content = strings.ReplaceAll(content, a.Old, a.New)
	} else {
		content = strings.Replace(content, a.Old, a.New, 1)
	}
	if len(content) > writeMaxBytes {
		return errOut(rel, fmt.Errorf("после правки файл больше %d байт", writeMaxBytes))
	}
	if err := t.writeRegular(rel, []byte(content), st.Mode().Perm()); err != nil {
		return errOut(rel, err)
	}
	return toolOutput{text: fmt.Sprintf("заменено вхождений в %s: %d", rel, n), detail: rel, files: []string{rel}}
}
