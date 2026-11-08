// Package session implements the caller-owned session file: a JSON-lines
// file holding fixed settings (line 1) and one API message per later line.
package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ErrLocked means another run holds the session file.
var ErrLocked = errors.New("session file is locked by another run")

// ErrEmpty means the session file has no header: a create was interrupted,
// or another run is still creating it.
var ErrEmpty = errors.New("session file is empty")

// Header is line 1 of the session file.
type Header struct {
	Type    string          `json:"type"`
	Format  int             `json:"format"`
	Model   string          `json:"model"`
	Effort  string          `json:"effort"`
	Sandbox string          `json:"sandbox"`
	System  string          `json:"system"`
	Tools   json.RawMessage `json:"tools"`
}

// Store receives messages as a run completes each step.
type Store interface {
	Append(json.RawMessage) error
}

// Discard is a Store that does nothing (runs without --session).
type Discard struct{}

func (Discard) Append(json.RawMessage) error { return nil }

// File is an open, locked session file.
type File struct {
	Header   Header
	Messages []json.RawMessage
	f        *os.File
	broken   bool
}

// Open creates the file with header h if missing, or loads it (ignoring h).
// The file stays locked until Close.
func Open(path string, h Header) (*File, bool, error) {
	// O_EXCL fails on any existing path, including a dangling symlink.
	fd, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		sf := &File{f: fd}
		if err := lock(fd); err != nil {
			fd.Close()
			return nil, false, err
		}
		h.Type, h.Format = "session", 1
		line, err := encodeHeader(h)
		if err == nil {
			_, err = fd.Write(line)
		}
		if err == nil {
			err = fd.Sync()
		}
		if err != nil {
			fd.Close()
			os.Remove(path) // best effort: never leave a headerless file
			return nil, false, fmt.Errorf("write session header: %w", err)
		}
		sf.Header = h
		return sf, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}
	fd, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false, err
	}
	if st, err := fd.Stat(); err != nil || !st.Mode().IsRegular() {
		fd.Close()
		return nil, false, fmt.Errorf("session path %s is not a regular file", path)
	}
	if err := lock(fd); err != nil {
		fd.Close()
		return nil, false, err
	}
	sf := &File{f: fd}
	if err := sf.load(); err != nil {
		fd.Close()
		return nil, false, err
	}
	return sf, false, nil
}

func lock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLocked
		}
		return err
	}
	return nil
}

// encodeHeader marshals without HTML escaping; callers pass compact Tools.
func encodeHeader(h Header) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(h); err != nil {
		return nil, err
	}
	return b.Bytes(), nil // Encode appends the newline
}

func (s *File) load() error {
	data, err := io.ReadAll(s.f)
	if err != nil {
		return err
	}
	// valid is the length of the data kept; a partial final line is dropped.
	valid := len(data)
	if i := bytes.LastIndexByte(data, '\n'); i < len(data)-1 {
		tail := data[i+1:]
		if json.Valid(tail) {
			// complete line, only the newline is missing
			if _, err := s.f.Write([]byte("\n")); err != nil {
				return err
			}
			data = append(data, '\n')
			valid = len(data)
		} else {
			valid = i + 1
			data = data[:valid]
		}
	}
	if valid == 0 {
		// Empty: a create was interrupted, or another run is still creating
		// it (header not yet written).
		return ErrEmpty
	}
	lines := bytes.Split(data[:valid-1], []byte("\n"))
	if err := json.Unmarshal(lines[0], &s.Header); err != nil {
		return fmt.Errorf("session header: %w", err)
	}
	if s.Header.Type != "session" {
		return errors.New("session header: wrong type")
	}
	if s.Header.Format != 1 {
		return fmt.Errorf("unsupported session format %d", s.Header.Format)
	}
	for i, l := range lines[1:] {
		var m struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(l, &m); err != nil || m.Type != "message" || len(m.Message) == 0 {
			return fmt.Errorf("session line %d is corrupt", i+2)
		}
		s.Messages = append(s.Messages, m.Message)
	}
	if err := s.f.Truncate(int64(valid)); err != nil {
		return err
	}
	_, err = s.f.Seek(0, io.SeekEnd)
	return err
}

// Append writes one message line and syncs. msg is stored byte-for-byte.
func (s *File) Append(msg json.RawMessage) error {
	if s.broken {
		return errors.New("session file is broken by an earlier write failure")
	}
	if bytes.IndexByte(msg, '\n') >= 0 {
		return errors.New("session message contains a newline")
	}
	if !json.Valid(msg) {
		return errors.New("session message is not valid JSON")
	}
	line := make([]byte, 0, len(msg)+40)
	line = append(line, `{"type":"message","message":`...)
	line = append(line, msg...)
	line = append(line, "}\n"...)
	off, err := s.f.Seek(0, io.SeekCurrent)
	if err != nil {
		s.broken = true
		return err
	}
	if _, err := s.f.Write(line); err != nil {
		s.broken = true
		s.f.Truncate(off) // best effort: drop any partial line
		return err
	}
	if err := s.f.Sync(); err != nil {
		s.broken = true
		return err
	}
	s.Messages = append(s.Messages, append(json.RawMessage(nil), msg...))
	return nil
}

// Close releases the lock.
func (s *File) Close() error { return s.f.Close() }
