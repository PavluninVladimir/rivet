// Package scm — единый интерфейс git-хостинга (спека backend/scm-integration).
// Конвейер не знает различий PR/MR — их прячет адаптер.
package scm

import "context"

type PR struct {
	Number int
	URL    string
}

type Adapter interface {
	// CreatePR создаёт pull request из ветки задачи в базовую ветку.
	CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error)
	// Diff возвращает diff PR (контекст для review-агента).
	Diff(ctx context.Context, repo string, number int) (string, error)
	// Merge выполняет merge PR и возвращает sha merge-коммита
	// (версия для автопубликаций, спека backend/deployment).
	Merge(ctx context.Context, repo string, number int) (string, error)
	// HeadSHA — текущий HEAD ветки (версия для ручного запуска публикации).
	HeadSHA(ctx context.Context, repo, branch string) (string, error)
}
