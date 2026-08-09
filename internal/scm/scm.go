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
	// Merge выполняет merge PR.
	Merge(ctx context.Context, repo string, number int) error
}
