package redact

import "bytes"

// maxTail — предел буфера неполной строки: длинный вывод без переводов
// строк (прогресс-бары, минифицированный текст) не должен ни задерживать
// live-трансляцию неограниченно, ни копить память.
const maxTail = 64 << 10

// keepTail — сколько байт хвоста удерживается при принудительном сбросе
// длинной строки: секрет, разрезанный границей сброса, остаётся в буфере
// целиком и будет замаскирован при доборе. 8 КБ покрывает и длинные
// bearer-значения (JWT); многострочные PEM ведёт inPEM-состояние. Секрет
// длиннее keepTail внутри строки длиннее maxTail — за пределами модели.
const keepTail = 8 << 10

// Stream — построчный редактор потока вывода одной сессии. Чанки приходят
// разрезанными в произвольных местах; всё до последнего разделителя строки
// маскируется и уходит сразу, хвост придерживается до следующего чанка или
// Flush. Разделители — \n и \r (прогресс-бары агентов идут через \r).
// Не потокобезопасен: вызывающий сериализует Feed/Flush одной сессии.
type Stream struct {
	tail  []byte
	inPEM bool
}

// Feed принимает очередной чанк и возвращает замаскированную часть,
// готовую к публикации (может быть пустой).
func (st *Stream) Feed(chunk []byte) []byte {
	buf := append(st.tail, chunk...)
	cut := lastSep(buf) + 1
	var out []byte
	if cut > 0 {
		out = st.maskLines(buf[:cut])
	}
	st.tail = append([]byte(nil), buf[cut:]...)
	if len(st.tail) > maxTail {
		// Принудительный сброс длинной строки: последние keepTail байт
		// остаются в буфере, чтобы секрет на границе сброса не ушёл
		// в live-поток разрезанным (и потому непойманным).
		emit := len(st.tail) - keepTail
		out = append(out, st.maskPartial(st.tail[:emit])...)
		st.tail = append([]byte(nil), st.tail[emit:]...)
	}
	return out
}

// Flush маскирует и отдаёт остаток буфера; состояние сбрасывается
// (конец стадии — конец сессии редактора).
func (st *Stream) Flush() []byte {
	out := st.maskPartial(st.tail)
	st.tail = nil
	st.inPEM = false
	return out
}

// lastSep — индекс последнего разделителя строки, -1 если его нет.
func lastSep(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' || b[i] == '\r' {
			return i
		}
	}
	return -1
}

// maskLines маскирует завершённые строки, сохраняя разделители.
func (st *Stream) maskLines(b []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(b))
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' && b[i] != '\r' {
			continue
		}
		out.WriteString(st.maskLine(string(b[start:i])))
		out.WriteByte(b[i])
		start = i + 1
	}
	return out.Bytes()
}

// maskPartial маскирует неполную строку (принудительный сброс хвоста).
func (st *Stream) maskPartial(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return []byte(st.maskLine(string(b)))
}

// maskLine маскирует одну строку с учётом PEM-состояния: тело блока
// приватного ключа заменяется маской целиком.
func (st *Stream) maskLine(line string) string {
	if st.inPEM {
		if pemEnd.MatchString(line) {
			st.inPEM = false
			return applyRules(line)
		}
		return mask
	}
	if pemBegin.MatchString(line) {
		if !pemEnd.MatchString(line) {
			st.inPEM = true
		}
		return String(line)
	}
	return applyRules(line)
}
