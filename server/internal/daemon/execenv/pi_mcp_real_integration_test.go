//go:build agentintegration

package execenv

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// Real-CLI acceptance for Pi project MCP. Opt-in twice: the agentintegration
// build tag, and MULTICA_RUN_REAL_AGENT_SMOKE, because this executes the pi
// binary installed on the host. HOME points at a temp directory so the test
// does not read or write the user's ~/.pi. Pi exits before connecting MCP
// when the temp home has no model credential, so the fixture points --model
// at a local listener that accepts the request and never returns a completion.
// That is not the user's account. The oracle is a local stdio program that
// records its own start.
func TestPiMcpUntrustedProjectDoesNotStartServer(t *testing.T) {
	bin, workDir, sentinel := piMcpFixture(t)

	sessionPath := filepath.Join(workDir, "session.jsonl")
	if err := os.WriteFile(sessionPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The stub model never completes, so this context is what ends the process.
	// The wait is longer than the approved test needs to observe a start.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"-p", "--mode", "json", "--offline",
		"--session", sessionPath,
		"--model", piMcpStubModel,
	)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	cmd.Stdin = strings.NewReader("ping\n")
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start untrusted pi: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if started, readErr := os.ReadFile(sentinel); readErr == nil {
			t.Fatalf("untrusted pi started the MCP server (%q); output:\n%s", started, output.String())
		}
		select {
		case <-done:
			if started, readErr := os.ReadFile(sentinel); readErr == nil {
				t.Fatalf("untrusted pi started the MCP server (%q); output:\n%s", started, output.String())
			}
			if strings.Contains(output.String(), "No API key") {
				t.Fatalf("untrusted pi exited before project trust was tested:\n%s", output.String())
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestPiMcpApprovedLaunchStartsServer(t *testing.T) {
	bin, workDir, sentinel := piMcpFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	backend, err := agent.New("pi", agent.Config{ExecutablePath: bin, Logger: logger})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	session, err := backend.Execute(ctx, "ping", agent.ExecOptions{
		Cwd:        workDir,
		Model:      piMcpStubModel,
		CustomArgs: []string{"--offline"},
	})
	if err != nil {
		t.Fatalf("execute pi: %v", err)
	}
	resultCh := make(chan agent.Result, 1)
	go func() {
		for range session.Messages {
		}
		result, ok := <-session.Result
		if ok {
			resultCh <- result
		}
	}()

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(sentinel); readErr == nil && strings.TrimSpace(string(data)) == "started" {
			cancel()
			return
		}
		select {
		case result := <-resultCh:
			t.Fatalf("pi exited before starting the MCP server: status=%s error=%s output=%s", result.Status, result.Error, result.Output)
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("approved pi did not start the MCP server")
}

func piMcpFixture(t *testing.T) (bin, workDir, sentinel string) {
	t.Helper()
	if os.Getenv("MULTICA_RUN_REAL_AGENT_SMOKE") != "1" {
		t.Skip("set MULTICA_RUN_REAL_AGENT_SMOKE=1 to allow real agent CLI access")
	}
	if testing.Short() {
		t.Skip("skipping real-binary smoke test in -short mode")
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell-script MCP fixture is POSIX-only")
	}

	var err error
	bin, err = exec.LookPath("pi")
	if err != nil {
		t.Skip("pi not on PATH; skipping real-binary smoke test")
	}
	if version, verErr := exec.Command(bin, "--version").CombinedOutput(); verErr == nil {
		t.Logf("pi CLI version: %s", strings.TrimSpace(string(version)))
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writePiStubModel(t, home)

	workDir = t.TempDir()
	sentinel = filepath.Join(workDir, "probe.log")
	probe := filepath.Join(workDir, "probe.sh")
	script := "#!/bin/sh\nprintf 'started\\n' >> \"$PI_MCP_PROBE_LOG\"\nsleep 30\n"
	if err := os.WriteFile(probe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"probe": map[string]any{
				"command": probe,
				"env":     map[string]string{"PI_MCP_PROBE_LOG": sentinel},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := preparePiMcpConfig(workDir, "pi", raw, &sidecarManifest{}); err != nil {
		t.Fatalf("prepare pi mcp config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".pi", "mcp.json")); err != nil {
		t.Fatalf("project mcp.json missing: %v", err)
	}
	return bin, workDir, sentinel
}

const piMcpStubModel = "localstub/stub"

// writePiStubModel registers a provider that speaks to a listener on
// 127.0.0.1. The listener accepts one request and then holds the connection,
// so Pi gets past its API-key check without calling a hosted model.
func writePiStubModel(t *testing.T, home string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hold, stop := context.WithCancel(context.Background())
	t.Cleanup(func() {
		stop()
		_ = ln.Close()
	})
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				<-hold.Done()
			}(conn)
		}
	}()

	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"providers": map[string]any{
			"localstub": map[string]any{
				"baseUrl": "http://" + ln.Addr().String() + "/v1",
				"api":     "openai-completions",
				"apiKey":  "local-stub",
				"models": []any{map[string]any{
					"id":            "stub",
					"contextWindow": 8000,
					"maxTokens":     256,
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
