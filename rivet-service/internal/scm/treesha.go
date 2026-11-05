package scm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"path"
	"strings"
)

// TreeHasher — SHA дерева каталога на версии ref. Пусто без ошибки —
// каталога нет. Снимок архитектуры сравнивает его с прошлым чтением и при
// совпадении не перечитывает описание целиком (merge, не трогающий
// docs/architecture, стоит одного запроса).
type TreeHasher interface {
	PathSHA(ctx context.Context, repo, ref, dir string) (string, error)
}

func splitDir(dir string) (parent, name string) {
	dir = strings.Trim(dir, "/")
	return path.Dir(dir), path.Base(dir)
}

// PathSHA — GitHub: содержимое родительского каталога, sha записи-каталога.
func (g *GitHub) PathSHA(ctx context.Context, repo, ref, dir string) (string, error) {
	parent, name := splitDir(dir)
	url := "/repos/" + repo + "/contents"
	if parent != "." {
		url += "/" + escapePath(parent)
	}
	url += "?ref=" + neturl.QueryEscape(ref)
	raw, code, err := g.do(ctx, "GET", url, nil, "")
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "", nil
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("github contents %s: %d: %s", parent, code, tailMsg(raw))
	}
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return "", nil // родитель — файл, а не каталог: каталога описания нет
	}
	for _, e := range entries {
		if e.Name == name && e.Type == "dir" {
			return e.SHA, nil
		}
	}
	return "", nil
}

// PathSHA — GitLab: дерево родительского каталога, id записи-дерева.
func (g *GitLab) PathSHA(ctx context.Context, repo, ref, dir string) (string, error) {
	parent, name := splitDir(dir)
	for page := 1; page <= 5; page++ {
		q := neturl.Values{"ref": {ref}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if parent != "." {
			q.Set("path", parent)
		}
		raw, code, err := g.do(ctx, "GET", "/projects/"+projectID(repo)+"/repository/tree?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		if code == http.StatusNotFound {
			return "", nil
		}
		if code != http.StatusOK {
			return "", fmt.Errorf("gitlab tree %s: %d: %s", parent, code, tailMsg(raw))
		}
		var entries []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return "", err
		}
		for _, e := range entries {
			if e.Name == name && e.Type == "tree" {
				return e.ID, nil
			}
		}
		if len(entries) < 100 {
			return "", nil
		}
	}
	return "", nil
}

// PathSHA — fake в git-режиме: настоящий SHA дерева. Без git-режима
// сравнивать нечего, снимок читается целиком.
func (f *Fake) PathSHA(ctx context.Context, repo, ref, dir string) (string, error) {
	gitDir, inGit := f.repoDir(repo)
	if !inGit {
		return "", nil
	}
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("fake git: недопустимая версия %q", ref)
	}
	sha, ok, err := revParse(ctx, gitDir, ref+":"+strings.Trim(dir, "/"))
	if err != nil || !ok {
		return "", err
	}
	return sha, nil
}
