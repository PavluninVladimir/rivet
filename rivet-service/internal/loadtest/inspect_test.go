package loadtest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublicationInspectionIdentity(t *testing.T) {
	target := Target{ProjectID: "project", EnvironmentID: "env", Binding: Binding{PublicationID: "publication", Version: "sha", Generation: 7}, ContainerID: strings.Repeat("a", 64), Port: 25100, ContainerPort: 8080}
	original := map[string]any{"Id": target.ContainerID, "State": map[string]any{"Running": true}, "Config": map[string]any{"Labels": map[string]string{"rivet.managed": "true", "rivet.project": "project", "rivet.environment": "env", "rivet.publication": "publication", "rivet.version": "sha", "rivet.generation": "7"}}, "HostConfig": map[string]any{"PortBindings": map[string]any{"8080/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "25100"}}}}}
	raw, _ := json.Marshal([]any{original})
	if err := validateInspection(target, raw); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(map[string]any){func(m map[string]any) { m["Id"] = "other" }, func(m map[string]any) { m["State"].(map[string]any)["Running"] = false }, func(m map[string]any) {
		m["Config"].(map[string]any)["Labels"].(map[string]any)["rivet.version"] = "new"
	}, func(m map[string]any) {
		m["Config"].(map[string]any)["Labels"].(map[string]any)["rivet.generation"] = "8"
	}, func(m map[string]any) {
		m["HostConfig"].(map[string]any)["PortBindings"].(map[string]any)["8080/tcp"].([]any)[0].(map[string]any)["HostIp"] = "0.0.0.0"
	}, func(m map[string]any) {
		m["HostConfig"].(map[string]any)["PortBindings"].(map[string]any)["8080/tcp"].([]any)[0].(map[string]any)["HostPort"] = "25101"
	}} {
		var fixtures []map[string]any
		_ = json.Unmarshal(raw, &fixtures)
		mutation(fixtures[0])
		bad, _ := json.Marshal(fixtures)
		if validateInspection(target, bad) == nil {
			t.Fatal("changed publication accepted", string(bad))
		}
	}
}
