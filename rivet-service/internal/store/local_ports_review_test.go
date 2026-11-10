package store

import (
	"context"
	"fmt"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"sync"
	"testing"
)

func TestLocalPortsAllocatedAtomically(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "ports", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	const count = 8
	ports := make(chan int, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := s.CreateEnvironment(ctx, domain.Environment{ProjectID: p.ID, Name: fmt.Sprintf("local-%d", i), ExecType: "ssh", Trigger: "manual", Config: domain.EnvConfig{Provision: &domain.ProvisionConfig{Provider: domain.ProvisionLocal}}})
			if err != nil {
				errs <- err
				return
			}
			ports <- e.Config.Provision.Port
		}()
	}
	wg.Wait()
	close(ports)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	used := map[int]bool{}
	for port := range ports {
		if port == 0 || used[port] {
			t.Fatalf("порт не уникален: %d", port)
		}
		used[port] = true
	}
	if len(used) != count {
		t.Fatalf("получено %d портов", len(used))
	}
	for port := range used {
		_, err := s.CreateEnvironment(ctx, domain.Environment{ProjectID: p.ID, Name: "manual", ExecType: "ssh", Trigger: "manual", Config: domain.EnvConfig{Provision: &domain.ProvisionConfig{Provider: domain.ProvisionLocal, Port: port}}})
		if err == nil {
			t.Fatal("занятый ручной порт принят")
		}
		break
	}
}
