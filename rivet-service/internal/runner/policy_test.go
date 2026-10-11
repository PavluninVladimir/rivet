package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
)

// Доставка политики runner'у (change add-policy-delivery, спека
// access-policy): политика приезжает в назначении и доходит до агента
// промптом; другого источника у runner'а нет.

func TestPolicyPrompt(t *testing.T) {
	as := &pb.Assignment{
		TaskNum: 7, Branch: "agent/task-7", Title: "A", Description: "d",
		Policy: &pb.Policy{
			Hash:             "0123456789abcdef0123",
			HumanReviewPaths: []string{"infra/**", "deploy/prod.yaml"},
			PolicyDir:        ".rivet/",
		},
	}
	prompt := stagePrompt(as)
	for _, want := range []string{"Политика проекта (версия 0123456789ab)", ".rivet/", "infra/**", "deploy/prod.yaml"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("в промпте нет %q:\n%s", want, prompt)
		}
	}
	// Промпт review тоже несёт политику: ревьюер должен знать защищённые пути.
	if !strings.Contains(reviewPrompt(as), "Политика проекта") {
		t.Fatal("промпт review без политики")
	}
	// Сессия доработки человека — тот же блок.
	withUser := &pb.Assignment{TaskNum: 7, Branch: as.Branch, Policy: as.Policy, UserPrompt: "почини тест"}
	if !strings.Contains(stagePrompt(withUser), "Политика проекта") {
		t.Fatal("промпт сессии доработки без политики")
	}

	// Назначение без политики промпт не меняет.
	bare := &pb.Assignment{TaskNum: 7, Branch: as.Branch, Title: "A"}
	if strings.Contains(stagePrompt(bare), "Политика проекта") {
		t.Fatal("без политики блока быть не должно")
	}
	empty := &pb.Assignment{TaskNum: 7, Branch: as.Branch, Title: "A", Policy: &pb.Policy{}}
	if strings.Contains(stagePrompt(empty), "Политика проекта") {
		t.Fatal("пустая версия — блока быть не должно")
	}

	// Длинный список путей обрезается счётчиком.
	many := make([]string, policyPathsInPrompt+5)
	for i := range many {
		many[i] = "path" + strings.Repeat("x", i%3) + "/**"
	}
	long := &pb.Assignment{TaskNum: 7, Branch: as.Branch, Title: "A",
		Policy: &pb.Policy{Hash: "abc", HumanReviewPaths: many}}
	if !strings.Contains(stagePrompt(long), "и ещё 5 защищённых путей") {
		t.Fatalf("длинный список должен сворачиваться:\n%s", stagePrompt(long))
	}
}

// Файл политики в рабочей копии на исполнение не влияет: runner его не
// читает (сценарий «Подмена политики в рабочей копии»).
func TestPolicyFromAssignmentOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".rivet"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Подложенная «политика» разрешает всё и называет чужую версию.
	if err := os.WriteFile(filepath.Join(dir, ".rivet", "policy.yaml"),
		[]byte("auto_merge: true\nhuman_review_paths: []\nhash: podmena\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	as := &pb.Assignment{
		TaskNum: 1, Branch: "agent/task-1", Title: "A",
		Policy: &pb.Policy{Hash: "trusted-hash", HumanReviewPaths: []string{"infra/**"}, PolicyDir: ".rivet/"},
	}
	prompt := stagePrompt(as)
	if strings.Contains(prompt, "podmena") {
		t.Fatalf("политика из рабочей копии просочилась в промпт:\n%s", prompt)
	}
	if !strings.Contains(prompt, "trusted-hash"[:12]) && !strings.Contains(prompt, "trusted-hash") {
		t.Fatalf("в промпте должна быть версия из назначения:\n%s", prompt)
	}
	if !strings.Contains(prompt, "infra/**") {
		t.Fatalf("защищённые пути из назначения:\n%s", prompt)
	}
}
