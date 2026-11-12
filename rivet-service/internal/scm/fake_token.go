package scm

import (
	"context"
	"time"
)

// FakeToken — fake-хостинг, увиденный через токен проекта: состояние общее
// с Fake, а отозванный токен получает 401 на любой операции. Так стенд
// различает «токен отозвали» и «токен заменили» (fix-credential-state).
type FakeToken struct {
	*Fake
	token string
}

// ForToken — fake-хостинг для токена проекта.
func (f *Fake) ForToken(token string) *FakeToken { return &FakeToken{Fake: f, token: token} }

func (t *FakeToken) refusedToken() error {
	if t.tokenRevoked(t.token) {
		return errFakeRevoked
	}
	return nil
}

func (t *FakeToken) CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error) {
	if err := t.refusedToken(); err != nil {
		return PR{}, err
	}
	return t.Fake.CreatePR(ctx, repo, branch, base, title, body)
}

func (t *FakeToken) Diff(ctx context.Context, repo string, number int) (string, error) {
	if err := t.refusedToken(); err != nil {
		return "", err
	}
	return t.Fake.Diff(ctx, repo, number)
}

func (t *FakeToken) Merge(ctx context.Context, repo string, number int) (string, error) {
	if err := t.refusedToken(); err != nil {
		return "", err
	}
	return t.Fake.Merge(ctx, repo, number)
}

func (t *FakeToken) MergeAt(ctx context.Context, repo string, number int, headSHA string) (string, error) {
	if err := t.refusedToken(); err != nil {
		return "", err
	}
	return t.Fake.MergeAt(ctx, repo, number, headSHA)
}

func (t *FakeToken) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	if err := t.refusedToken(); err != nil {
		return "", err
	}
	return t.Fake.HeadSHA(ctx, repo, branch)
}

func (t *FakeToken) Probe(ctx context.Context, repo string) ProbeResult {
	if t.tokenRevoked(t.token) {
		return probeFail(ReasonBadToken, "токен не принят хостингом")
	}
	return t.Fake.Probe(ctx, repo)
}

func (t *FakeToken) CreateRepo(ctx context.Context, in NewRepo) (RepoInfo, error) {
	if err := t.refusedToken(); err != nil {
		return RepoInfo{}, err
	}
	return t.Fake.CreateRepo(ctx, in)
}

func (t *FakeToken) RegisterWebhook(ctx context.Context, repo, url, secret string) (bool, error) {
	if err := t.refusedToken(); err != nil {
		return false, err
	}
	return t.Fake.RegisterWebhook(ctx, repo, url, secret)
}

func (t *FakeToken) TriggerPipeline(ctx context.Context, repo, pipeline, ref string, vars map[string]string) (PipelineRun, error) {
	if err := t.refusedToken(); err != nil {
		return PipelineRun{}, err
	}
	return t.Fake.TriggerPipeline(ctx, repo, pipeline, ref, vars)
}

func (t *FakeToken) PipelineRun(ctx context.Context, repo, pipeline, ref, runID string, since time.Time) (PipelineRun, error) {
	if err := t.refusedToken(); err != nil {
		return PipelineRun{}, err
	}
	return t.Fake.PipelineRun(ctx, repo, pipeline, ref, runID, since)
}

func (t *FakeToken) ReadFile(ctx context.Context, repo, ref, path string) (FileContent, error) {
	if err := t.refusedToken(); err != nil {
		return FileContent{}, err
	}
	return t.Fake.ReadFile(ctx, repo, ref, path)
}

func (t *FakeToken) WriteFile(ctx context.Context, repo, ref, path, prevID, content, message string) (Commit, error) {
	if err := t.refusedToken(); err != nil {
		return Commit{}, err
	}
	return t.Fake.WriteFile(ctx, repo, ref, path, prevID, content, message)
}

func (t *FakeToken) BranchProtected(ctx context.Context, repo, branch string) (bool, error) {
	if err := t.refusedToken(); err != nil {
		return false, err
	}
	return t.Fake.BranchProtected(ctx, repo, branch)
}

func (t *FakeToken) PRFiles(ctx context.Context, repo string, number int) (PRFileList, error) {
	if err := t.refusedToken(); err != nil {
		return PRFileList{}, err
	}
	return t.Fake.PRFiles(ctx, repo, number)
}

func (t *FakeToken) PathSHA(ctx context.Context, repo, ref, dir string) (string, error) {
	if err := t.refusedToken(); err != nil {
		return "", err
	}
	return t.Fake.PathSHA(ctx, repo, ref, dir)
}
