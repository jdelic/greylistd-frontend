// Package logging sets up the structured JSON application logger and the
// HTTP access log middleware.
package logging

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// ParseLevel converts a level name into a slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	err := l.UnmarshalText([]byte(strings.ToUpper(strings.TrimSpace(s))))
	if err != nil {
		return 0, fmt.Errorf("invalid log level %q", s)
	}
	return l, nil
}

// NewApp returns the application logger (JSON lines on w, normally stderr).
func NewApp(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}

// NewAccess returns the access logger (JSON lines on w, normally stdout).
func NewAccess(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// Every access log line is info-level; the level adds nothing.
			if len(groups) == 0 && a.Key == slog.LevelKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// AccessLog wraps h and writes one JSON line per request to log. clientIP
// extracts the client address (it may honor proxy headers).
func AccessLog(log *slog.Logger, clientIP func(*http.Request) string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		log.Info("request",
			slog.String("client", clientIP(r)),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("proto", r.Proto),
			slog.Int("status", rec.status),
			slog.Int64("bytes", rec.bytes),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.String("user_agent", r.UserAgent()),
			slog.String("referer", r.Referer()),
		)
	})
}
