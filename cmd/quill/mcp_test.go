package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"quill/internal/events"
	"quill/internal/fakeapi"
	"quill/internal/fakemcp"
)

const mcpToken = "tok-51c2-secret"

// runQuillEnv is runQuill with extra environment variables for quill.
func runQuillEnv(t *testing.T, fake *fakeapi.Server, env []string, stdin string, args ...string) result {
	t.Helper()
	cmd := quillCmd(fake, t.TempDir(), stdin, append([]string{"exec"}, append(args, "-")...)...)
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatalf("run quill: %v", err)
	}
	r := result{code: cmd.ProcessState.ExitCode(), stdout: out.String(), stderr: errb.String()}
	r.lines = parseLines(t, r.stdout)
	checkStream(t, r.code, r.lines)
	return r
}

func toolNames(t *testing.T, defs json.RawMessage) []string {
	t.Helper()
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(defs, &list); err != nil {
		t.Fatalf("tools: %v", err)
	}
	names := make([]string, len(list))
	for i, d := range list {
		names[i] = d.Name
	}
	return names
}

// toolResults returns the tool results in the last message of an API request.
func toolResults(t *testing.T, body []byte) []struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
} {
	t.Helper()
	req := request(t, body)
	var m struct {
		Content []struct {
			Content string `json:"content"`
			IsError bool   `json:"is_error"`
		} `json:"content"`
	}
	if err := json.Unmarshal(req.Messages[len(req.Messages)-1], &m); err != nil {
		t.Fatalf("message: %v", err)
	}
	return m.Content
}

// A new session lists the server's tools once and fixes them in the header;
// continuing uses the stored list against whatever server the caller names.
func TestE2EMCPSessionAndResume(t *testing.T) {
	for _, profile := range []string{"read-only", "read-write"} {
		t.Run(profile, func(t *testing.T) {
			builtins := 5
			if profile == "read-write" {
				builtins = 8
			}
			ws, sess := t.TempDir(), filepath.Join(t.TempDir(), "s.jsonl")
			srv1 := fakemcp.New(t, fakemcp.Tool{Name: "zeta"}, fakemcp.Tool{Name: "echo", Description: "Echo the arguments."})
			fake1 := fakeapi.New(t,
				fakeapi.ToolReply("tu1", "mcp__echo", `{"text":"hi"}`),
				fakeapi.TextReply("done", events.Usage{}),
			)
			env := []string{"QUILL_MCP_TOKEN=" + mcpToken}
			r := runQuillEnv(t, fake1, env, "task", "--workspace", ws, "--session", sess, "--tools", profile, "--mcp-url", srv1.URL())
			if r.code != 0 {
				t.Fatalf("run 1 exit = %d; stdout: %s stderr: %s", r.code, r.stdout, r.stderr)
			}
			if got, _ := json.Marshal(r.lines[0]["mcp_tools"]); string(got) != `["mcp__echo","mcp__zeta"]` {
				t.Errorf("run.started mcp_tools = %s", got)
			}
			reqs1 := fake1.Requests()
			req1 := request(t, reqs1[0])
			names := toolNames(t, req1.Tools)
			if len(names) != builtins+2 || names[builtins] != "mcp__echo" || names[builtins+1] != "mcp__zeta" {
				t.Errorf("tools sent = %v", names)
			}
			if !strings.Contains(string(req1.Tools), `"description":"Echo the arguments."`) {
				t.Errorf("tools lack the server's description: %s", req1.Tools)
			}
			if res := toolResults(t, reqs1[1]); len(res) != 1 || res[0].IsError || res[0].Content != `{"text":"hi"}` {
				t.Errorf("tool result = %+v", res)
			}
			for _, a := range srv1.Auth() {
				if a != "Bearer "+mcpToken {
					t.Fatalf("Authorization = %q", a)
				}
			}

			// Run 2: another server with other tools. Nothing is listed again.
			srv2 := fakemcp.New(t, fakemcp.Tool{Name: "other"}, fakemcp.Tool{Name: "echo", Description: "Changed."})
			fake2 := fakeapi.New(t,
				fakeapi.ToolReply("tu2", "mcp__echo", `{"n":2}`),
				fakeapi.ToolReply("tu3", "mcp__other", `{}`),
				fakeapi.TextReply("done", events.Usage{}),
			)
			r = runQuillEnv(t, fake2, env, "more", "--workspace", ws, "--session", sess, "--mcp-url", srv2.URL())
			if r.code != 0 {
				t.Fatalf("run 2 exit = %d; stdout: %s stderr: %s", r.code, r.stdout, r.stderr)
			}
			if got, _ := json.Marshal(r.lines[0]["mcp_tools"]); string(got) != `["mcp__echo","mcp__zeta"]` {
				t.Errorf("run 2 mcp_tools = %s", got)
			}
			reqs2 := fake2.Requests()
			if req2 := request(t, reqs2[0]); !bytes.Equal(req1.Tools, req2.Tools) {
				t.Errorf("tools differ between runs:\nrun 1: %s\nrun 2: %s", req1.Tools, req2.Tools)
			}
			if res := toolResults(t, reqs2[1]); len(res) != 1 || res[0].IsError || res[0].Content != `{"n":2}` {
				t.Errorf("run 2 tool result = %+v", res)
			}
			// A tool the session does not have is unknown, even if the server has it.
			if res := toolResults(t, reqs2[2]); len(res) != 1 || res[0].Content != "unknown tool: mcp__other" {
				t.Errorf("unlisted tool result = %+v", res)
			}

			// The token and the server URLs are in no output and not in the session.
			file, err := os.ReadFile(sess)
			if err != nil {
				t.Fatal(err)
			}
			for what, text := range map[string]string{"stdout": r.stdout, "stderr": r.stderr, "session file": string(file)} {
				for _, private := range []string{mcpToken, srv1.URL(), srv2.URL()} {
					if strings.Contains(text, private) {
						t.Errorf("%s contains %q", what, private)
					}
				}
			}

			// Run 3: the session has MCP tools, so --mcp-url is required.
			r = runQuillEnv(t, fakeapi.New(t), env, "more", "--workspace", ws, "--session", sess)
			if r.code != 2 || !strings.Contains(r.stderr, "--mcp-url") {
				t.Errorf("resume without --mcp-url: exit %d, stderr %q", r.code, r.stderr)
			}
		})
	}
}

func TestE2EMCPCannotBeAddedOnResume(t *testing.T) {
	ws, sess := t.TempDir(), filepath.Join(t.TempDir(), "s.jsonl")
	r := runQuill(t, fakeapi.New(t, fakeapi.TextReply("done", events.Usage{})), "task", "--workspace", ws, "--session", sess)
	if r.code != 0 {
		t.Fatalf("run 1 exit = %d; stderr: %s", r.code, r.stderr)
	}
	if got, _ := json.Marshal(r.lines[0]["mcp_tools"]); string(got) != `[]` {
		t.Errorf("run.started mcp_tools = %s, want []", got)
	}
	srv := fakemcp.New(t, fakemcp.Tool{Name: "echo"})
	r = runQuill(t, fakeapi.New(t), "more", "--workspace", ws, "--session", sess, "--mcp-url", srv.URL())
	if r.code != 2 || !strings.Contains(r.stderr, "--mcp-url") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	if len(srv.Auth()) != 0 {
		t.Error("the server was contacted")
	}
}

// The token never reaches tools: not by default, and --env refuses it.
func TestE2EMCPTokenNotPassedToTools(t *testing.T) {
	srv := fakemcp.New(t, fakemcp.Tool{Name: "echo"})
	fake := fakeapi.New(t,
		fakeapi.ToolReply("tu1", "shell", `{"command":"env"}`),
		fakeapi.TextReply("done", events.Usage{}),
	)
	env := []string{"QUILL_MCP_TOKEN=" + mcpToken}
	r := runQuillEnv(t, fake, env, "task", "--workspace", t.TempDir(), "--mcp-url", srv.URL())
	if r.code != 0 {
		t.Fatalf("exit = %d; stderr: %s", r.code, r.stderr)
	}
	res := toolResults(t, fake.Requests()[1])
	if len(res) != 1 || !strings.Contains(res[0].Content, "PATH=") {
		t.Fatalf("shell result = %+v", res)
	}
	if strings.Contains(res[0].Content, "QUILL_MCP_TOKEN") || strings.Contains(r.stdout, mcpToken) {
		t.Errorf("the token reached a tool or stdout: %s", res[0].Content)
	}

	r = runQuillEnv(t, fakeapi.New(t), env, "task", "--workspace", t.TempDir(), "--mcp-url", srv.URL(), "--env", "QUILL_MCP_TOKEN")
	if r.code != 2 {
		t.Errorf("--env QUILL_MCP_TOKEN: exit %d, want 2", r.code)
	}
}

func TestE2EMCPStartFailure(t *testing.T) {
	down := fakemcp.New(t, fakemcp.Tool{Name: "echo"})
	down.Close()
	denied := fakemcp.New(t, fakemcp.Tool{Name: "echo"})
	denied.SetStatus(401)
	badName := fakemcp.New(t, fakemcp.Tool{Name: "has.dot"})
	noTools := fakemcp.New(t)
	for name, tc := range map[string]struct {
		srv        *fakemcp.Server
		httpStatus float64
	}{
		"unreachable": {down, 0}, "http 401": {denied, 401}, "bad tool name": {badName, 0}, "no tools": {noTools, 0},
	} {
		t.Run(name, func(t *testing.T) {
			sess := filepath.Join(t.TempDir(), "s.jsonl")
			fake := fakeapi.New(t)
			r := runQuillEnv(t, fake, []string{"QUILL_MCP_TOKEN=" + mcpToken}, "task",
				"--workspace", t.TempDir(), "--session", sess, "--mcp-url", tc.srv.URL())
			if r.code != 6 {
				t.Fatalf("exit = %d, want 6; stdout: %s stderr: %s", r.code, r.stdout, r.stderr)
			}
			l := last(t, r)
			e, _ := l["error"].(map[string]any)
			if l["status"] != "mcp_error" || e == nil || e["kind"] != "mcp_unavailable" || e["message"] == "" || e["http_status"] != tc.httpStatus {
				t.Errorf("run.completed = %v", l)
			}
			if len(r.lines) != 2 {
				t.Errorf("got %d stdout lines, want run.started and run.completed", len(r.lines))
			}
			if len(fake.Requests()) != 0 {
				t.Error("the model was called")
			}
			if _, err := os.Stat(sess); !os.IsNotExist(err) {
				t.Errorf("a session file was left behind (stat err %v)", err)
			}
			if out := r.stdout + r.stderr; strings.Contains(out, mcpToken) || strings.Contains(out, tc.srv.URL()) {
				t.Errorf("output carries the token or URL: %s", out)
			}
		})
	}
}

// Continuing a session with an unreachable server fails at the start too.
func TestE2EMCPStartFailureOnResume(t *testing.T) {
	ws, sess := t.TempDir(), filepath.Join(t.TempDir(), "s.jsonl")
	srv := fakemcp.New(t, fakemcp.Tool{Name: "echo"})
	r := runQuill(t, fakeapi.New(t, fakeapi.TextReply("done", events.Usage{})), "task", "--workspace", ws, "--session", sess, "--mcp-url", srv.URL())
	if r.code != 0 {
		t.Fatalf("run 1 exit = %d; stderr: %s", r.code, r.stderr)
	}
	before, _ := os.ReadFile(sess)
	srv.Close()
	r = runQuill(t, fakeapi.New(t), "more", "--workspace", ws, "--session", sess, "--mcp-url", srv.URL())
	if l := last(t, r); r.code != 6 || l["status"] != "mcp_error" {
		t.Errorf("exit = %d, run.completed = %v", r.code, l)
	}
	if got, _ := json.Marshal(r.lines[0]["mcp_tools"]); string(got) != `["mcp__echo"]` {
		t.Errorf("run.started mcp_tools = %s", got)
	}
	if after, _ := os.ReadFile(sess); !bytes.Equal(before, after) {
		t.Error("the failed run changed the session file")
	}
}

// A server that fails mid-run costs the model a tool error, not the run.
func TestE2EMCPFailureMidRun(t *testing.T) {
	var srv *fakemcp.Server
	srv = fakemcp.New(t,
		fakemcp.Tool{Name: "echo"},
		fakemcp.Tool{Name: "refuse", Call: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
			r := fakemcp.Text("not allowed")
			r.IsError = true
			return r, nil
		}},
		fakemcp.Tool{Name: "break", Call: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
			srv.SetStatus(500) // every later request fails
			return fakemcp.Text("ok"), nil
		}},
	)
	fake := fakeapi.New(t,
		fakeapi.ToolReply("tu1", "mcp__refuse", `{}`),
		fakeapi.ToolReply("tu2", "mcp__break", `{}`),
		fakeapi.ToolReply("tu3", "mcp__echo", `{}`),
		fakeapi.TextReply("gave up on the server", events.Usage{}),
	)
	r := runQuill(t, fake, "task", "--workspace", t.TempDir(), "--tools", "read-only", "--mcp-url", srv.URL())
	if l := last(t, r); r.code != 0 || l["status"] != "completed" {
		t.Fatalf("exit = %d, run.completed = %v; stderr: %s", r.code, l, r.stderr)
	}
	reqs := fake.Requests()
	if res := toolResults(t, reqs[1]); len(res) != 1 || !res[0].IsError || res[0].Content != "not allowed" {
		t.Errorf("server tool error = %+v", res)
	}
	if res := toolResults(t, reqs[2]); len(res) != 1 || res[0].IsError || res[0].Content != "ok" {
		t.Errorf("break result = %+v", res)
	}
	res := toolResults(t, reqs[3])
	if len(res) != 1 || !res[0].IsError || !strings.HasPrefix(res[0].Content, "MCP call failed: ") {
		t.Errorf("result after the failure = %+v", res)
	}
	if strings.Contains(r.stdout, srv.URL()) {
		t.Error("stdout carries the server URL")
	}
}

func TestE2EMCPBadURLIsBadUsage(t *testing.T) {
	for _, u := range []string{"stdio://x", "example.com/mcp", "http://user:pw@example.com/mcp", "file:///tmp/x"} {
		r := runQuill(t, fakeapi.New(t), "task", "--workspace", t.TempDir(), "--mcp-url", u)
		if r.code != 2 || r.stdout != "" {
			t.Errorf("%s: exit %d, stdout %q", u, r.code, r.stdout)
		}
		if strings.Contains(r.stderr, "pw") {
			t.Errorf("%s: stderr echoes the URL: %q", u, r.stderr)
		}
	}
}
