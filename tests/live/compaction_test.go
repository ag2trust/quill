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
	"os"
	"path/filepath"
	"strings"
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

// liveReply is what the test keeps of one 200 response.
type liveReply struct {
	blocks   []string // content block types, in order
	summary  bool     // the compaction block has a non-null summary
	top      events.Usage
	compact  events.Usage // sum of the compaction iterations
	message  events.Usage // sum of the message iterations
	iters    int          // entries in usage.iterations
	compacts int
}

type liveUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (u liveUsage) usage() events.Usage {
	return events.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
}

func parseLiveReply(body []byte) (liveReply, error) {
	var msg struct {
		Content []struct {
			Type    string  `json:"type"`
			Content *string `json:"content"`
		} `json:"content"`
		Usage struct {
			liveUsage
			Iterations []struct {
				Type string `json:"type"`
				liveUsage
			} `json:"iterations"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return liveReply{}, err
	}
	r := liveReply{top: msg.Usage.usage()}
	for _, b := range msg.Content {
		r.blocks = append(r.blocks, b.Type)
		if b.Type == "compaction" && b.Content != nil {
			r.summary = true
		}
	}
	r.iters = len(msg.Usage.Iterations)
	for _, it := range msg.Usage.Iterations {
		switch it.Type {
		case "compaction":
			r.compacts++
			r.compact.Add(it.usage())
		case "message":
			r.message.Add(it.usage())
		}
	}
	return r, nil
}

// TestLiveCompaction runs the loop in-process, because the trigger is not a
// flag, with the trigger at the API minimum. The task reads enough text to
// pass it and then keeps using tools. It checks that the API compacts on a
// non-streaming request with client tools, that the run continues with the
// whole history (thinking included) replayed, and that the compaction call is
// counted in the run's usage.
func TestLiveCompaction(t *testing.T) {
	live(t)
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Twelve files of about 26 KB (under the 30 KB tool output cap), each
	// hiding one code line. Seeded, so the text is the same on every run.
	const files = 12
	rng := rand.New(rand.NewSource(1))
	words := strings.Fields("amber basin cedar delta ember fjord grove harbor inlet juniper kettle lagoon " +
		"meadow nectar orchard prairie quarry ridge summit thicket upland valley willow yarrow zenith")
	var codes []string
	for i := 1; i <= files; i++ {
		var b strings.Builder
		code := fmt.Sprintf("%s-%s-%d", words[rng.Intn(len(words))], words[rng.Intn(len(words))], 100+rng.Intn(900))
		codes = append(codes, code)
		at := 100 + rng.Intn(150)
		for n := 0; b.Len() < 26000; n++ {
			if n == at {
				fmt.Fprintf(&b, "CODE: %s\n", code)
			}
			fmt.Fprintf(&b, "entry %d:", n)
			for w := 0; w < 9; w++ {
				b.WriteString(" " + words[rng.Intn(len(words))])
			}
			b.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(ws, fmt.Sprintf("part%02d.txt", i)), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	task := fmt.Sprintf("The workspace holds part01.txt to part%02d.txt. Each has exactly one line starting with "+
		"\"CODE: \". Read every file in full with read_file, in order, one file per step (a single tool call "+
		"per step; do not use search or the shell). After each file, state the code you found. When all are "+
		"read, write codes.txt with the %d codes, one per line, in file order. Then reply done.", files, files)

	var replies []liveReply
	capture := func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
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
			replies = append(replies, lr)
		}
		return res, nil
	}
	client := provider.NewClaude(os.Getenv("ANTHROPIC_API_KEY"), option.WithMiddleware(capture))
	client.CompactTrigger = 50000 // the API minimum

	env, _ := proc.BuildEnv(os.Getenv, nil, t.TempDir())
	runner := proc.NewRunner(ws, env)
	defer runner.KillAll(time.Second)
	registry := tools.NewRegistry(tools.ReadWrite, ws, tools.NewShell(runner, time.Minute))
	first, err := prompt.FirstMessage(ws, task)
	if err != nil {
		t.Fatal(err)
	}
	var evbuf bytes.Buffer
	loop := &agent.Loop{
		Client:   client,
		Registry: registry,
		Store:    session.Discard{},
		Events:   events.NewWriter(&evbuf),
		Opts: agent.Options{
			// High effort so the model is likely to think: thinking blocks in
			// turns after the compaction block are then replayed.
			Model: "claude-haiku-5-5", Effort: "high", System: prompt.System(), Tools: registry.Definitions(),
			MaxSteps: 30, MaxTokens: 1500000,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res := loop.Run(ctx, ctx, first)

	var want events.Usage
	compactAt, compactions := -1, 0
	var thinkBefore, thinkAfter, toolStepsAfter int
	for i, r := range replies {
		want.Add(r.top)
		want.Add(r.compact)
		// Quill adds the compaction iterations to the top-level counts. That
		// is only right if the top level, cache fields included, is exactly
		// the message iterations; this is read from the raw reply, not from
		// the provider's own sums.
		if r.iters == 0 {
			t.Errorf("reply %d has no usage.iterations", i)
		} else if r.top != r.message {
			t.Errorf("reply %d top-level usage = %+v, want the sum of its message iterations %+v (compaction iterations %+v)",
				i, r.top, r.message, r.compact)
		}
		compactions += r.compacts
		isCompact := len(r.blocks) > 0 && r.blocks[0] == "compaction"
		if isCompact && compactAt < 0 {
			compactAt = i
			t.Logf("reply %d compacted (summary present: %v); blocks %v; top-level %+v; compaction iteration %+v",
				i, r.summary, r.blocks, r.top, r.compact)
		}
		for _, b := range r.blocks {
			switch {
			case b == "thinking" || b == "redacted_thinking":
				if compactAt < 0 {
					thinkBefore++
				} else {
					thinkAfter++
				}
			case b == "tool_use" && compactAt >= 0 && i > compactAt:
				toolStepsAfter++
			}
		}
	}
	t.Logf("status %s, steps %d, replies %d, compaction iterations %d, usage %+v", res.Status, res.Steps, len(replies), compactions, res.Usage)
	t.Logf("thinking blocks: %d before the compaction reply, %d in or after it; tool calls after it: %d", thinkBefore, thinkAfter, toolStepsAfter)
	if res.Error != nil {
		t.Logf("error: %+v", *res.Error)
	}

	if res.Status != events.StatusCompleted {
		t.Fatalf("status = %s, want completed", res.Status)
	}
	if compactAt < 0 {
		t.Fatalf("no reply started with a compaction block")
	}
	if !replies[compactAt].summary {
		t.Errorf("the compaction block has no summary")
	}
	if replies[compactAt].compact.Input <= 0 || replies[compactAt].compact.Output <= 0 {
		t.Errorf("compaction iteration usage = %+v, want input and output > 0", replies[compactAt].compact)
	}
	if toolStepsAfter < 2 {
		t.Errorf("tool calls after compaction = %d, want >= 2", toolStepsAfter)
	}
	if res.Usage != want {
		t.Errorf("run usage = %+v, want %+v (top-level plus compaction iterations)", res.Usage, want)
	}
	// History stays append-only: the first message and the compaction turn are both still there.
	if len(loop.History) == 0 || !bytes.Equal(loop.History[0], first) {
		t.Errorf("history no longer starts with the first message")
	}
	kept := 0
	for _, m := range loop.History {
		var v struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		if json.Unmarshal(m, &v) == nil && len(v.Content) > 0 && v.Content[0].Type == "compaction" {
			kept++
		}
	}
	if kept == 0 {
		t.Errorf("no compaction block in the history")
	}
	// Informational: how well the summary carried the early codes.
	got, _ := os.ReadFile(filepath.Join(ws, "codes.txt"))
	found := 0
	for _, c := range codes {
		if strings.Contains(string(got), c) {
			found++
		}
	}
	t.Logf("codes.txt holds %d of %d codes", found, len(codes))
}
