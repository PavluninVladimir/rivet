package loadtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestK6VersionGateAtStartupAndBeforeTraffic(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		accepted     bool
		exitFailure  bool
	}{
		{name: "legacy", output: "k6 v2.3.0 (go1.26, linux/amd64)\n", accepted: true},
		{name: "legacy patch", output: "k6 v2.3.12\n", accepted: true},
		{name: "homebrew", output: "k6 v2.4.0 (commit/devel, go1.27.4, darwin/arm64)\n", accepted: true},
		{name: "new patch", output: "k6 v2.4.12", accepted: true},
		{name: "old family", output: "k6 v2.2.0\n"},
		{name: "future minor", output: "k6 v2.5.0\n"},
		{name: "future major", output: "k6 v3.0.0\n"},
		{name: "release candidate", output: "k6 v2.4.0-rc.1\n"},
		{name: "build suffix", output: "k6 v2.4.0+custom\n"},
		{name: "extra version segment", output: "k6 v2.4.0.1\n"},
		{name: "malformed", output: "k6 v2.4.bad\n"},
		{name: "unrelated prefix", output: "warning: k6 v2.4.0\n"},
		{name: "empty"},
		{name: "oversized", output: "k6 v2.4.0 " + strings.Repeat("x", 4096)},
		{name: "command failure", output: "k6 v2.4.0\n", exitFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "k6")
			// Fixed fixture output only. Runtime must reject invalid versions
			// before inspecting a publication or sending preflight traffic.
			script := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(tc.output, "'", "'\"'\"'") + "'\n"
			if tc.exitFailure {
				script += "exit 1\n"
			}
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			err := CheckBinary(context.Background(), bin)
			if (err == nil) != tc.accepted {
				t.Fatalf("startup accepted=%v: %v", tc.accepted, err)
			}
			if tc.accepted {
				return // Real execution of supported k6 is covered by HTTP fixtures.
			}
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			inspections := 0
			result := (Runtime{K6Bin: bin, Inspect: func(context.Context, Target) error {
				inspections++
				return nil
			}}).Execute(context.Background(), fixtureJob(t, server.URL, testProfile()))
			if result.State != "config_needed" || result.Reason == nil || result.Reason.Code != "k6_version" {
				t.Fatalf("invalid version ran: %+v", result)
			}
			if requests.Load() != 0 || inspections != 0 || result.K6Version != "" {
				t.Fatalf("invalid version reached publication: requests=%d inspections=%d version=%q", requests.Load(), inspections, result.K6Version)
			}
		})
	}
}
