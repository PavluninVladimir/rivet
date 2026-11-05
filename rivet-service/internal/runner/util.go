package runner

import (
	"crypto/rand"
	"encoding/hex"
)

// Общие хелперы runner'а (вынесены из удалённых адаптеров сторонних
// агентов, add-native-agent).

func newMsgID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
