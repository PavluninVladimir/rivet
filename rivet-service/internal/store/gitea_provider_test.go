package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Миграция 0042 (add-gitea-adapter): проект и учётные данные с провайдером
// gitea сохраняются, неизвестный провайдер по-прежнему отклоняется.
func TestGiteaProviderStored(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "owner-gitea", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProjectWithRepo(ctx, "gitea", nil, owner.ID, NewRepoConnection{
		Provider: "gitea", BaseURL: "https://gitea.example.com", RepoPath: "own/proj",
		Token: "fake-gitea-token-not-real", TokenOwner: "bot"}, box)
	if err != nil {
		t.Fatalf("проект на Gitea: %v", err)
	}
	if got, err := s.GetProject(ctx, p.ID); err != nil || got.Provider != "gitea" || got.BaseURL != "https://gitea.example.com" {
		t.Fatalf("проект: %v %+v", err, got)
	}
	if c, err := s.ProjectCredential(ctx, p.ID); err != nil || c.Provider != "gitea" {
		t.Fatalf("учётные данные: %v %+v", err, c)
	}
	if _, err := s.CreateProjectWithRepo(ctx, "bad", nil, owner.ID, NewRepoConnection{
		Provider: "bitbucket", BaseURL: "https://bitbucket.org", RepoPath: "own/x"}, box); err == nil {
		t.Fatal("неизвестный провайдер должен отклоняться базой")
	}
}
