package localdocker

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GarbageCollect читает только объекты конкретного окружения. Проверка меток
// повторяется после list: широкая очистка Docker и force image prune запрещены.
func (r *Runtime) GarbageCollect(ctx context.Context, j Job) {
	if j.validate() != nil {
		return
	}
	keep := map[string]bool{}
	if !j.GarbageCollectionOnly {
		keep[j.PublicationID] = true
	}
	for _, id := range j.KeepPublications {
		keep[id] = true
	}
	filters := []string{"--filter", "label=rivet.managed=true", "--filter", "label=rivet.project=" + j.ProjectID, "--filter", "label=rivet.environment=" + j.EnvironmentID}
	args := append([]string{"container", "ls", "--all", "--no-trunc"}, filters...)
	args = append(args, "--format", "{{.ID}}")
	raw, e := r.call(ctx, args...)
	if e != nil {
		return
	}
	protectedImages := map[string]bool{}
	for _, id := range strings.Fields(raw) {
		if !containerID.MatchString(id) {
			continue
		}
		data, e := r.call(ctx, "container", "inspect", id)
		if e != nil {
			continue
		}
		var a []Inspection
		if json.Unmarshal([]byte(data), &a) != nil || len(a) != 1 {
			continue
		}
		c := a[0]
		if c.ID != id || !scopeOwned(c.Config.Labels, j) {
			continue
		}
		if keep[c.Config.Labels["rivet.publication"]] {
			protectedImages[c.Image] = true
			continue
		}
		if c.Created.IsZero() || time.Since(c.Created) < 10*time.Minute {
			protectedImages[c.Image] = true
			continue
		}
		// Останавливаем с обычным grace, затем удаляем по проверенному полному ID.
		_, e = r.call(ctx, "container", "stop", "--time", strconv.Itoa(j.Config.StopTimeoutS), id)
		if e == nil {
			_, _ = r.call(ctx, "container", "rm", id)
		} else {
			protectedImages[c.Image] = true
		}
	}
	args = append([]string{"image", "ls", "--no-trunc", "--quiet"}, filters...)
	raw, e = r.call(ctx, args...)
	if e != nil {
		return
	}
	type image struct {
		ID         string
		generation int64
	}
	var images []image
	seen := map[string]bool{}
	for _, id := range strings.Fields(raw) {
		if !imageID.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		data, e := r.call(ctx, "image", "inspect", id)
		if e != nil {
			continue
		}
		var a []struct {
			ID     string
			Config struct{ Labels map[string]string }
		}
		if json.Unmarshal([]byte(data), &a) != nil || len(a) != 1 || a[0].ID != id || !scopeOwned(a[0].Config.Labels, j) {
			continue
		}
		g, e := strconv.ParseInt(a[0].Config.Labels["rivet.generation"], 10, 64)
		if e == nil {
			images = append(images, image{id, g})
		}
	}
	sort.Slice(images, func(a, b int) bool { return images[a].generation > images[b].generation })
	for i, im := range images {
		if i < 2 || protectedImages[im.ID] {
			continue
		}
		_, _ = r.call(ctx, "image", "rm", im.ID)
	}
}
func scopeOwned(labels map[string]string, j Job) bool {
	return labels["rivet.managed"] == "true" && labels["rivet.project"] == j.ProjectID && labels["rivet.environment"] == j.EnvironmentID
}

func (r *Runtime) removePublication(ctx context.Context, j Job) Result {
	if j.RemoveData {
		return r.removeEnvironment(ctx, j)
	}
	// Повтор после потери ответа может встретить уже удалённый объект.
	raw, e := r.call(ctx, "container", "inspect", j.name())
	if e == nil {
		var a []Inspection
		if json.Unmarshal([]byte(raw), &a) != nil || len(a) != 1 || !owned(a[0], j) {
			return result(j, "unknown", fail("config_invalid", "контейнер удаления не принадлежит публикации"))
		}
		c := a[0]
		if c.State.Running {
			return result(j, "unknown", fail("config_invalid", "перед удалением остановите публикацию"))
		}
		if _, e = r.call(ctx, "container", "rm", c.ID); e != nil {
			return result(j, "unknown", e)
		}
	}
	if j.Image != "" {
		raw, e = r.call(ctx, "image", "inspect", j.Image)
		if e == nil {
			var a []struct {
				ID     string
				Config struct{ Labels map[string]string }
			}
			if json.Unmarshal([]byte(raw), &a) != nil || len(a) != 1 || !scopeOwned(a[0].Config.Labels, j) {
				return result(j, "unknown", fail("config_invalid", "образ удаления не принадлежит окружению"))
			}
			if _, e = r.call(ctx, "image", "rm", a[0].ID); e != nil {
				return result(j, "unknown", e)
			}
		}
	}
	if j.RemoveData {
		name, e := r.volume(ctx, j)
		if e != nil {
			return result(j, "unknown", e)
		}
		if _, e = r.call(ctx, "volume", "rm", name); e != nil {
			return result(j, "unknown", e)
		}
	}
	return result(j, "stopped", nil)
}

// removeEnvironment вызывается только для подтверждённого удаления
// остановленного окружения. Захватывает и объекты, созданные перед сбоем
// runner-а, чьи идентификаторы ещё не успели вернуться в control plane.
func (r *Runtime) removeEnvironment(ctx context.Context, j Job) Result {
	filters := []string{"--filter", "label=rivet.managed=true", "--filter", "label=rivet.project=" + j.ProjectID, "--filter", "label=rivet.environment=" + j.EnvironmentID}
	args := append([]string{"container", "ls", "--all", "--no-trunc"}, filters...)
	args = append(args, "--format", "{{.ID}}")
	raw, e := r.call(ctx, args...)
	if e != nil {
		return result(j, "unknown", e)
	}
	for _, id := range strings.Fields(raw) {
		if !containerID.MatchString(id) {
			return result(j, "unknown", fail("config_invalid", "некорректный ID контейнера очистки"))
		}
		data, e := r.call(ctx, "container", "inspect", id)
		if e != nil {
			return result(j, "unknown", e)
		}
		var a []Inspection
		if json.Unmarshal([]byte(data), &a) != nil || len(a) != 1 || a[0].ID != id || !scopeOwned(a[0].Config.Labels, j) {
			return result(j, "unknown", fail("config_invalid", "контейнер очистки не принадлежит окружению"))
		}
		if a[0].State.Running {
			if _, e = r.call(ctx, "container", "stop", "--time", strconv.Itoa(j.Config.StopTimeoutS), id); e != nil {
				return result(j, "unknown", e)
			}
		}
		if _, e = r.call(ctx, "container", "rm", id); e != nil {
			return result(j, "unknown", e)
		}
	}
	args = append([]string{"image", "ls", "--no-trunc", "--quiet"}, filters...)
	raw, e = r.call(ctx, args...)
	if e != nil {
		return result(j, "unknown", e)
	}
	seen := map[string]bool{}
	for _, id := range strings.Fields(raw) {
		if seen[id] {
			continue
		}
		seen[id] = true
		if !imageID.MatchString(id) {
			return result(j, "unknown", fail("config_invalid", "некорректный ID образа очистки"))
		}
		data, e := r.call(ctx, "image", "inspect", id)
		if e != nil {
			return result(j, "unknown", e)
		}
		var a []struct {
			ID     string
			Config struct{ Labels map[string]string }
		}
		if json.Unmarshal([]byte(data), &a) != nil || len(a) != 1 || a[0].ID != id || !scopeOwned(a[0].Config.Labels, j) {
			return result(j, "unknown", fail("config_invalid", "образ очистки не принадлежит окружению"))
		}
		if _, e = r.call(ctx, "image", "rm", id); e != nil {
			return result(j, "unknown", e)
		}
	}
	name, e := r.volume(ctx, j)
	if e != nil {
		return result(j, "unknown", e)
	}
	if _, e = r.call(ctx, "volume", "rm", name); e != nil {
		return result(j, "unknown", e)
	}
	out := result(j, "stopped", nil)
	out.ResourcesRemoved = true
	return out
}
