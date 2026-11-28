package logreader

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/actions-gateway/github-actions-gateway/oomreporter/internal/report"
)

// Sender sends lines to the agent's socket at Path, dialling again after a failure.
type Sender struct {
	Path string
	conn net.Conn
}

// Send sends l.
func (s *Sender) Send(l report.Line) error {
	b, err := l.MarshalBinary()
	if err != nil {
		return err
	}
	if s.conn == nil {
		if s.conn, err = net.Dial("unix", s.Path); err != nil {
			s.conn = nil
			return err
		}
	}
	if _, err = s.conn.Write(b); err != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	return err
}

// Serve hands got each line sent to a socket at path until ctx is done. A connection that sends
// what is no line is closed.
func Serve(ctx context.Context, path string, got func(report.Line), log *slog.Logger) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Go(func() {
			defer func() { _ = conn.Close() }()
			closed := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer closed()
			buf := make([]byte, report.LineSize)
			for {
				if _, err := io.ReadFull(conn, buf); err != nil {
					return
				}
				var l report.Line
				if err := l.UnmarshalBinary(buf); err != nil {
					log.Warn("closed the log reader's connection", "error", err)
					return
				}
				got(l)
			}
		})
	}
}
