package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func testHeader() Header {
	return Header{
		Model: "claude-opus-5-5", Effort: "high", Profile: "read-write",
		System: "sys <b> & \"q\"\nline2",
		Tools:  json.RawMessage(`[{"name":"a<b>","x":"&"}]`),
	}
}

var (
	msg1 = json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hi <&>"}]}`)
	msg2 = json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"yo"}]}`)
)

func create(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	f, created, err := Open(p, testHeader())
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	if err := f.Append(msg1); err != nil {
		t.Fatal(err)
	}
	if err := f.Append(msg2); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func TestCreateThenReopen(t *testing.T) {
	p := create(t)
	f, created, err := Open(p, Header{})
	if err != nil || created {
		t.Fatalf("reopen: %v %v", created, err)
	}
	defer f.Close()
	h := testHeader()
	if f.Header.Type != "session" || f.Header.Format != Format || f.Header.Model != h.Model ||
		f.Header.Effort != h.Effort || f.Header.Profile != h.Profile || f.Header.System != h.System {
		t.Fatalf("header: %+v", f.Header)
	}
	if !bytes.Equal(f.Header.Tools, h.Tools) {
		t.Fatalf("tools: %s", f.Header.Tools)
	}
	if len(f.Messages) != 2 || !bytes.Equal(f.Messages[0], msg1) || !bytes.Equal(f.Messages[1], msg2) {
		t.Fatalf("messages: %q", f.Messages)
	}
	if err := f.Append(msg1); err != nil || len(f.Messages) != 3 {
		t.Fatal("append after reopen", err)
	}
}

func TestMode0600(t *testing.T) {
	p := create(t)
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestLocked(t *testing.T) {
	p := create(t)
	f, _, err := Open(p, Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(p, Header{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	f.Close()
	g, _, err := Open(p, Header{})
	if err != nil {
		t.Fatalf("after close: %v", err)
	}
	g.Close()
}

func TestTruncatedLastLine(t *testing.T) {
	p := create(t)
	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(`{"type":"mess`)
	fh.Close()
	f, _, err := Open(p, Header{})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Messages) != 2 {
		t.Fatalf("messages %d", len(f.Messages))
	}
	if err := f.Append(msg1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b, _ := os.ReadFile(p)
	if b[len(b)-1] != '\n' {
		t.Fatalf("bad file: %q", b)
	}
	g, _, err := Open(p, Header{})
	if err != nil || len(g.Messages) != 3 {
		t.Fatalf("reopen: %v", err)
	}
	g.Close()
}

func TestTruncatedLastLineEndsWithNewline(t *testing.T) {
	p := create(t)
	fh, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	fh.WriteString(`{"type":"mess`)
	fh.Close()
	f, _, _ := Open(p, Header{})
	f.Close()
	b, _ := os.ReadFile(p)
	if b[len(b)-1] != '\n' {
		t.Fatalf("file does not end with newline: %q", b)
	}
}

func TestCorruptMiddleLine(t *testing.T) {
	p := create(t)
	b, _ := os.ReadFile(p)
	lines := bytes.Split(b, []byte("\n"))
	lines[1] = []byte(`{"type":"mess`)
	os.WriteFile(p, bytes.Join(lines, []byte("\n")), 0o600)
	if _, _, err := Open(p, Header{}); err == nil {
		t.Fatal("want error")
	}
	// lock must be released after a failed open
	os.WriteFile(p, b, 0o600)
	f, _, err := Open(p, Header{})
	if err != nil {
		t.Fatalf("lock leaked: %v", err)
	}
	f.Close()
}

func TestHeaderIgnoredOnReopen(t *testing.T) {
	p := create(t)
	f, _, err := Open(p, Header{Model: "other", System: "other"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Header.Model != "claude-opus-5-5" {
		t.Fatalf("header: %+v", f.Header)
	}
}

func TestBadFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s")
	os.WriteFile(p, []byte(`{"type":"session","format":1}`+"\n"), 0o600)
	if _, _, err := Open(p, Header{}); err == nil {
		t.Fatal("want error")
	}
}

func TestCreateRefusesDanglingSymlink(t *testing.T) {
	d := t.TempDir()
	target := filepath.Join(d, "target")
	link := filepath.Join(d, "link")
	os.Symlink(target, link)
	if _, _, err := Open(link, testHeader()); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("target was created through symlink")
	}
}

func TestOpenRefusesSymlinkToExisting(t *testing.T) {
	p := create(t)
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(p, link)
	if _, _, err := Open(link, Header{}); err == nil {
		t.Fatal("want error")
	}
}

func TestAppendRejectsNewline(t *testing.T) {
	p := create(t)
	f, _, _ := Open(p, Header{})
	defer f.Close()
	if err := f.Append(json.RawMessage("{\"a\":\n1}")); err == nil {
		t.Fatal("want error")
	}
	if len(f.Messages) != 2 {
		t.Fatal("message recorded")
	}
}

func TestDiscard(t *testing.T) {
	var s Store = Discard{}
	if err := s.Append(msg1); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteLastLineMissingNewline(t *testing.T) {
	p := create(t)
	b, _ := os.ReadFile(p)
	if err := os.WriteFile(p, b[:len(b)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	f, _, err := Open(p, Header{})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Messages) != 2 || !bytes.Equal(f.Messages[1], msg2) {
		t.Fatalf("messages: %q", f.Messages)
	}
	f.Close()
	b2, _ := os.ReadFile(p)
	if !bytes.Equal(b, b2) {
		t.Fatalf("want newline added: %q", b2)
	}
}

func TestEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Zero bytes is an interrupted create: the session starts here.
	f, created, err := Open(p, testHeader())
	if err != nil || !created || f.Header.Model != testHeader().Model {
		t.Fatalf("created = %v, err = %v", created, err)
	}
	if err := f.Append(json.RawMessage(`{"role":"user"}`)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	g, created, err := Open(p, Header{})
	if err != nil || created || len(g.Messages) != 1 || g.Header.Model != testHeader().Model {
		t.Fatalf("reopen: created = %v, err = %v", created, err)
	}
	g.Close()
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
}

// A file that is not a session, even one unfinished line, is never overwritten.
func TestPartialFirstLineIsNotASession(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notes")
	if err := os.WriteFile(p, []byte("not a session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(p, testHeader()); err == nil {
		t.Fatal("opened a file with no complete header")
	}
	if b, _ := os.ReadFile(p); string(b) != "not a session" {
		t.Fatalf("file changed: %q", b)
	}
}

func TestRefusesFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(p, Header{}); err == nil {
		t.Fatal("want error")
	}
}

func TestAppendFailureMarksBroken(t *testing.T) {
	p := create(t)
	f, _, err := Open(p, Header{})
	if err != nil {
		t.Fatal(err)
	}
	f.f.Close() // force write failure
	if err := f.Append(msg1); err == nil {
		t.Fatal("want write error")
	}
	if err := f.Append(msg1); err == nil || len(f.Messages) != 2 {
		t.Fatal("want broken error and no recorded message")
	}
}

func TestOpenWithHeaderFailureLeavesNoFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	boom := errors.New("boom")
	if _, _, err := OpenWith(p, func() (Header, error) { return Header{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("file left behind (stat err %v)", err)
	}
	// The header function is not called for an existing file.
	if f, _, err := Open(p, testHeader()); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	f, created, err := OpenWith(p, func() (Header, error) { t.Error("header called"); return Header{}, nil })
	if err != nil || created {
		t.Fatalf("created = %v, err = %v", created, err)
	}
	f.Close()
}
