// e2e-gitops-env — окружение GitOps e2e-стенда (rivet-e2e, сценарий
// публикации через GitOps, fix-e2e-gitops-deploy). Отвечает версией,
// которая закоммичена в репозиторий конфигурации: GET /<путь файла> отдаёт
// содержимое файла в ветке -ref bare-репозитория -repo. Так ведёт себя
// приложение под GitOps-контроллером после синхронизации: работает на той
// версии, что записана в конфигурации. Файла нет — 404, такой версии
// окружение ещё не видело. Публикация на стенде коммитит версию через fake
// SCM в git-режиме, а Verify rivetd опрашивает этот адрес и ждёт в ответе
// публикуемую версию.
//
// Слушает -addr (по умолчанию свободный порт loopback) и после listen
// пишет фактический адрес в -addr-file: отдельный порт стенду и сценариям
// заводить не нужно. Только тестовая инфраструктура стенда, в сборку
// продукта не входит.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "адрес HTTP")
	repo := flag.String("repo", "", "bare-репозиторий конфигурации")
	ref := flag.String("ref", "main", "ветка конфигурации")
	addrFile := flag.String("addr-file", "", "файл, куда записывается адрес после listen")
	flag.Parse()
	if *repo == "" {
		fail("нужен -repo")
	}
	if *ref == "" || strings.HasPrefix(*ref, "-") || strings.ContainsAny(*ref, " :\t\n") {
		fail("некорректная ветка %q", *ref)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fail("listen %s: %v", *addr, err)
	}
	if *addrFile != "" {
		if err := writeAtomic(*addrFile, ln.Addr().String()+"\n"); err != nil {
			fail("файл адреса: %v", err)
		}
	}
	fmt.Fprintln(os.Stderr, "e2e-gitops-env:", ln.Addr())
	srv := &http.Server{Handler: newHandler(*repo, *ref), ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.Serve(ln))
}

// newHandler — версия окружения из файла конфигурации (вынесен для тестов).
func newHandler(repo, ref string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "только GET", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if !validPath(path) {
			http.Error(w, "ожидается путь файла от корня репозитория", http.StatusBadRequest)
			return
		}
		content, found, err := readFile(r.Context(), repo, ref, path)
		if err != nil {
			log.Printf("e2e-gitops-env: %s: %v", path, err)
			http.Error(w, "ошибка чтения конфигурации", http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "версии в конфигурации нет", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(content))
	})
}

// validPath — путь от корня репозитория: без пустых сегментов, «.» и «..»,
// без ведущего «-» и символов, которые git прочтёт как часть версии.
func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "-") || strings.ContainsAny(p, ":\\\x00") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// readFile — содержимое файла path в ветке ref. found=false — файла нет
// или по пути каталог.
func readFile(ctx context.Context, repo, ref, path string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sha, err := git(ctx, repo, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/"+ref+":"+path)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	sha = strings.TrimSpace(sha)
	kind, err := git(ctx, repo, "cat-file", "-t", sha)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(kind) != "blob" {
		return "", false, nil
	}
	content, err := git(ctx, repo, "cat-file", "blob", sha)
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

func git(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// writeAtomic — файл появляется целиком: стенд ждёт его появления и сразу
// читает.
func writeAtomic(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gitops-env-addr-")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "e2e-gitops-env: "+format+"\n", args...)
	os.Exit(1)
}
