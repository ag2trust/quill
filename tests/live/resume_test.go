//go:build live

package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"quill/internal/agent"
	"quill/internal/events"
	"quill/internal/proc"
	"quill/internal/prompt"
	"quill/internal/provider"
	"quill/internal/session"
	"quill/internal/tools"
)

// fakeStop starts a proxy for ANTHROPIC_BASE_URL. It answers request n
// (1-based) to /v1/messages itself, with an assistant reply of the given
// content and stop reason, and forwards every other request to the real API.
// Neither stop can be produced on demand: a refusal needs a harmful prompt,
// and an empty end_turn is rare. What is checked live is that the API accepts
// the session Quill leaves after one.
func fakeStop(t *testing.T, n int, stopReason, content string) *httptest.Server {
	t.Helper()
	target, err := url.Parse("https://api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" && int(count.Add(1)) == n {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req_fake")
			fmt.Fprintf(w, `{"id":"msg_fake","type":"message","role":"assistant","model":"claude-haiku-5-5",`+
				`"content":%s,"stop_reason":%q,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`,
				content, stopReason)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sessionMessages returns the messages saved in the session file.
func sessionMessages(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	f, _, err := session.Open(path, session.Header{})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer f.Close()
	return f.Messages
}

// TestLiveResumeAfterRefusal refuses the first step, which Quill does not
// save, so the session holds only the first user message. The resume then
// sends two consecutive user text messages.
func TestLiveResumeAfterRefusal(t *testing.T) {
	bin := live(t)
	ws, sess := t.TempDir(), sessionPath(t)
	srv := fakeStop(t, 1, "refusal", `[{"type":"text","text":"I can't help with that."}]`)
	const task = "Create hello.txt containing exactly: hi"
	r1 := run(t, bin, task, ws, sess, "ANTHROPIC_BASE_URL="+srv.URL)
	wantExit(t, r1, 4)
	if e, _ := completed(r1)["error"].(map[string]any); e["kind"] != "refused" {
		t.Fatalf("run 1 error = %v, want kind refused", e)
	}
	if n := len(sessionMessages(t, sess)); n != 1 {
		t.Fatalf("session holds %d messages after the refusal, want 1", n)
	}

	r2 := run(t, bin, task, ws, sess)
	if r2.code == 4 {
		t.Fatalf("resume got provider_error (exit 4), the API rejected the history; stderr: %s", r2.stderr)
	}
	wantExit(t, r2, 0)
	wantFile(t, ws, "hello.txt", "hi")
}

// TestLiveResumeAfterEmptyEndTurn answers the second step, after a real tool
// call, with an empty end_turn, which Quill does not save. The session ends
// with the tool result, and the resume adds a user text message after it.
func TestLiveResumeAfterEmptyEndTurn(t *testing.T) {
	bin := live(t)
	ws, sess := t.TempDir(), sessionPath(t)
	srv := fakeStop(t, 2, "end_turn", `[]`)
	r1 := run(t, bin, "Create a.txt containing exactly: a", ws, sess, "ANTHROPIC_BASE_URL="+srv.URL)
	wantExit(t, r1, 0)
	wantFile(t, ws, "a.txt", "a")
	if n := len(sessionMessages(t, sess)); n != 3 {
		t.Fatalf("session holds %d messages after the empty end_turn, want 3 (prompt, tool call, tool result)", n)
	}

	r2 := run(t, bin, "Now also create b.txt containing exactly: b", ws, sess)
	if r2.code == 4 {
		t.Fatalf("resume got provider_error (exit 4), the API rejected the history; stderr: %s", r2.stderr)
	}
	wantExit(t, r2, 0)
	wantFile(t, ws, "b.txt", "b")
}

// writeParts writes n files of about 26 KB, each hiding one "CODE: " line,
// and returns the codes. Seeded, so the text is the same on every run.
func writeParts(t *testing.T, ws string, n int) []string {
	t.Helper()
	rng := rand.New(rand.NewSource(2))
	words := strings.Fields("amber basin cedar delta ember fjord grove harbor inlet juniper kettle lagoon " +
		"meadow nectar orchard prairie quarry ridge summit thicket upland valley willow yarrow zenith")
	var codes []string
	for i := 1; i <= n; i++ {
		var b strings.Builder
		code := fmt.Sprintf("%s-%s-%d", words[rng.Intn(len(words))], words[rng.Intn(len(words))], 100+rng.Intn(900))
		codes = append(codes, code)
		at := 100 + rng.Intn(150)
		for k := 0; b.Len() < 26000; k++ {
			if k == at {
				fmt.Fprintf(&b, "CODE: %s\n", code)
			}
			fmt.Fprintf(&b, "entry %d:", k)
			for w := 0; w < 9; w++ {
				b.WriteString(" " + words[rng.Intn(len(words))])
			}
			b.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(ws, fmt.Sprintf("part%02d.txt", i)), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return codes
}

type phase struct {
	res     events.RunCompleted
	replies []liveReply
	starts  []string // per request: role/type of the first message's first block
}

// runPhase runs one task on the session file in-process, as quill exec
// --session would, with the compaction trigger at the API minimum.
func runPhase(t *testing.T, ws, sessPath, task string) phase {
	t.Helper()
	var p phase
	capture := func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		start := "unreadable"
		if r.GetBody != nil {
			if rb, gerr := r.GetBody(); gerr == nil {
				start = requestStart(rb)
			}
		}
		res, err := next(r)
		if err != nil || res == nil {
			return res, err
		}
		body, rerr := io.ReadAll(res.Body)
		res.Body.Close()
		res.Body = io.NopCloser(bytes.NewReader(body))
		if rerr != nil {
			return res, nil
		}
		if res.StatusCode != http.StatusOK {
			// The response only: request headers carry the API key.
			t.Logf("API %d request-id %s: %s", res.StatusCode, res.Header.Get("request-id"), body)
			return res, nil
		}
		if lr, perr := parseLiveReply(body); perr == nil {
			p.replies = append(p.replies, lr)
			p.starts = append(p.starts, start)
		}
		return res, nil
	}
	client := provider.NewClaude(os.Getenv("ANTHROPIC_API_KEY"), option.WithMiddleware(capture))
	client.CompactTrigger = 50000 // the API minimum

	env, _ := proc.BuildEnv(os.Getenv, nil, t.TempDir())
	runner := proc.NewRunner(ws, env)
	defer runner.KillAll(time.Second)
	registry := tools.NewRegistry(tools.ReadWrite, ws, tools.NewShell(runner, time.Minute))
	f, _, err := session.Open(sessPath, session.Header{
		// High effort so the model is likely to think, as in TestLiveCompaction.
		Model: "claude-haiku-5-5", Effort: "high", Profile: string(tools.ReadWrite),
		System: prompt.System(), Tools: registry.Definitions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var next json.RawMessage
	if len(f.Messages) == 0 {
		next, err = prompt.FirstMessage(ws, task)
	} else {
		next, err = prompt.ResumeMessage(f.Messages[len(f.Messages)-1], task)
	}
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{
		Client:   client,
		Registry: registry,
		Store:    f,
		Events:   events.NewWriter(io.Discard),
		Opts: agent.Options{
			Model: f.Header.Model, Effort: f.Header.Effort, System: f.Header.System, Tools: f.Header.Tools,
			MaxSteps: 30, MaxTokens: 1500000,
		},
		History: f.Messages,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p.res = loop.Run(ctx, ctx, next)

	t.Logf("status %s, steps %d, replies %d, first compacted reply %d, usage %+v; request starts %v",
		p.res.Status, p.res.Steps, len(p.replies), p.firstCompaction(), p.res.Usage, p.starts)
	if p.res.Error != nil {
		t.Logf("error: %+v", *p.res.Error)
	}
	return p
}

// firstCompaction returns the index of the first reply that compacted, or -1.
func (p phase) firstCompaction() int {
	for i, r := range p.replies {
		if len(r.blocks) > 0 && r.blocks[0] == "compaction" {
			return i
		}
	}
	return -1
}

// TestLiveCompactionResumed runs three tasks on one session file. Run 1
// stays under the trigger. Run 2 resumes it and crosses the trigger, so the
// compaction happens in a resumed run. Run 3 resumes a session that already
// holds a compaction with a summary, so every request starts at that message.
func TestLiveCompactionResumed(t *testing.T) {
	live(t)
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess := sessionPath(t)
	const files = 12
	codes := writeParts(t, ws, files)
	read := func(from, to int) string {
		return fmt.Sprintf("Read part%02d.txt to part%02d.txt in full with the read tool, in order, one file per step "+
			"(a single tool call per step; do not use search or the shell). After each file, state the code you found. "+
			"Then reply done.", from, to)
	}

	p1 := runPhase(t, ws, sess, fmt.Sprintf("The workspace holds part01.txt to part%02d.txt. Each has exactly one "+
		"line starting with \"CODE: \". ", files)+read(1, 3))
	if p1.res.Status != events.StatusCompleted {
		t.Fatalf("run 1 status = %s, want completed", p1.res.Status)
	}
	if p1.firstCompaction() >= 0 {
		t.Fatalf("run 1 compacted; it must stay under the trigger so the compaction happens after a resume")
	}
	n1 := len(sessionMessages(t, sess))

	p2 := runPhase(t, ws, sess, "Continue the same way: "+read(4, files))
	if p2.res.Status != events.StatusCompleted {
		t.Fatalf("run 2 status = %s, want completed", p2.res.Status)
	}
	at := p2.firstCompaction()
	if at < 0 {
		t.Fatalf("run 2 (resumed) never compacted")
	}
	if !p2.replies[at].summary {
		t.Fatalf("run 2 compaction block has no summary")
	}
	for i := at + 1; i < len(p2.starts); i++ {
		if p2.starts[i] != "assistant/compaction" {
			t.Errorf("run 2 request %d starts with %s, want assistant/compaction", i, p2.starts[i])
		}
	}
	msgs := sessionMessages(t, sess)
	saved := 0
	for _, m := range msgs {
		var v struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		if json.Unmarshal(m, &v) == nil && len(v.Content) > 0 && v.Content[0].Type == "compaction" {
			saved++
		}
	}
	if saved == 0 {
		t.Fatalf("no compaction block saved in the session file")
	}
	if len(msgs) <= n1 {
		t.Fatalf("session has %d messages after run 2, want more than run 1's %d", len(msgs), n1)
	}

	p3 := runPhase(t, ws, sess, "Write codes.txt with the codes you found, one per line, in file order. Then reply done.")
	if p3.res.Status != events.StatusCompleted {
		t.Fatalf("run 3 status = %s, want completed", p3.res.Status)
	}
	for i, s := range p3.starts {
		if s != "assistant/compaction" {
			t.Errorf("run 3 request %d starts with %s, want assistant/compaction (the saved summary)", i, s)
		}
	}
	// Informational: how well the summary carried the codes.
	got, _ := os.ReadFile(filepath.Join(ws, "codes.txt"))
	found := 0
	for _, c := range codes {
		if strings.Contains(string(got), c) {
			found++
		}
	}
	t.Logf("codes.txt holds %d of %d codes; session holds %d messages, %d compaction blocks", found, len(codes), len(sessionMessages(t, sess)), saved)
}
