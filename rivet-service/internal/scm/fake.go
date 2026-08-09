package scm

import (
	"context"
	"fmt"
	"sync/atomic"
)

// Fake — SCM-провайдер для e2e и локальных стендов (RIVET_SCM=fake).
// PR фиктивный, merge — no-op. Продуктовое поведение не меняет:
// включается только явным флагом окружения.
type Fake struct {
	seq atomic.Int64
}

func NewFake() *Fake { return &Fake{} }

func (f *Fake) CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error) {
	n := int(f.seq.Add(1))
	return PR{Number: n, URL: fmt.Sprintf("https://fake.local/%s/pull/%d", repo, n)}, nil
}

func (f *Fake) Diff(ctx context.Context, repo string, number int) (string, error) {
	return "diff --git a/e2e b/e2e\n+e2e", nil
}

func (f *Fake) Merge(ctx context.Context, repo string, number int) error { return nil }
