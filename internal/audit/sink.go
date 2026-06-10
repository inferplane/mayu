package audit

import (
	"io"
	"os"
)

// Sink consumes serialized audit records (one JSON object per Write, emitted
// as a newline-delimited line). Required sinks gate the failure policy (§5.4);
// non-required sinks (e.g. stdout) are best-effort.
type Sink interface {
	Write(rec []byte) error
	Name() string
	Required() bool
	Close() error
}

// WriterSink writes JSONL to any io.Writer (used for stdout and tests).
type WriterSink struct {
	name     string
	w        io.Writer
	required bool
}

func NewWriterSink(name string, w io.Writer, required bool) *WriterSink {
	return &WriterSink{name: name, w: w, required: required}
}

func (s *WriterSink) Write(rec []byte) error {
	if _, err := s.w.Write(rec); err != nil {
		return err
	}
	_, err := s.w.Write([]byte{'\n'})
	return err
}
func (s *WriterSink) Name() string   { return s.name }
func (s *WriterSink) Required() bool { return s.required }
func (s *WriterSink) Close() error   { return nil }

// NewStdoutSink is the default best-effort sink (§5.4: stdout required:false).
func NewStdoutSink() *WriterSink { return NewWriterSink("stdout", os.Stdout, false) }

// FileSink appends JSONL to a file (required by default).
type FileSink struct {
	name     string
	f        *os.File
	required bool
}

func NewFileSink(path string, required bool) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileSink{name: "file", f: f, required: required}, nil
}

func (s *FileSink) Write(rec []byte) error {
	if _, err := s.f.Write(rec); err != nil {
		return err
	}
	if _, err := s.f.Write([]byte{'\n'}); err != nil {
		return err
	}
	return s.f.Sync() // durability for the required audit sink
}
func (s *FileSink) Name() string   { return s.name }
func (s *FileSink) Required() bool { return s.required }
func (s *FileSink) Close() error   { return s.f.Close() }
