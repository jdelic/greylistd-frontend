package greylistd

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeServer answers greylistd commands with canned replies and records what
// it received.
type fakeServer struct {
	mu       sync.Mutex
	received []string
	replies  map[string]string
}

func startFake(t *testing.T, replies map[string]string) (*fakeServer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "socket")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fs := &fakeServer{replies: replies}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				b, _ := io.ReadAll(c)
				fs.mu.Lock()
				fs.received = append(fs.received, string(b))
				fs.mu.Unlock()
				_, _ = io.WriteString(c, fs.replies[string(b)])
			}(conn)
		}
	}()
	return fs, path
}

const listOutput = "\nWhitelist data:\n===============\n  Last Seen            Count  Data\n" +
	"  2026-11-21 09:56:52      1  example.com\n\nGreylist data:\n==============\n" +
	"  Last Seen            Count  Data\n  2026-11-21 09:56:52      1  1.2.3.4\n" +
	"  2026-11-21 10:00:00     12  1.2.3.5 sender@foo.org rcpt@bar.org\n\n" +
	"Blacklist data:\n===============\n  Last Seen            Count  Data\n" +
	"  2026-11-21 09:56:52      3  spam.test\n"

func TestList(t *testing.T) {
	_, path := startFake(t, map[string]string{"list": listOutput})
	c := New(path)
	c.Location = time.UTC
	entries, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{White, "example.com", time.Date(2026, 11, 21, 9, 56, 52, 0, time.UTC), 1},
		{Grey, "1.2.3.4", time.Date(2026, 11, 21, 9, 56, 52, 0, time.UTC), 1},
		{Grey, "1.2.3.5 sender@foo.org rcpt@bar.org", time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC), 12},
		{Black, "spam.test", time.Date(2026, 11, 21, 9, 56, 52, 0, time.UTC), 3},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, entries[i], want[i])
		}
	}
}

func TestListEmpty(t *testing.T) {
	_, path := startFake(t, map[string]string{
		"list": "error: Cannot read from '/var/lib/greylistd/triplets': [Errno 2] No such file or directory: '/var/lib/greylistd/triplets'\n       ",
	})
	entries, err := New(path).List(context.Background())
	if err != nil || len(entries) != 0 {
		t.Fatalf("got %v, %v", entries, err)
	}
}

func TestAddDelete(t *testing.T) {
	fs, path := startFake(t, map[string]string{
		"add --white example.com": "Added to whitelist",
		"add --black spam.test":   "Added to blacklist",
		"delete gone.test":        "error: Not found",
		"delete example.com":      "Removed from whitelist",
	})
	c := New(path)
	ctx := context.Background()
	if err := c.Add(ctx, White, "  Example.COM "); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(ctx, Black, "spam.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "gone.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := c.Add(ctx, White, "--grey x"); err == nil {
		t.Fatal("option injection was accepted")
	}
	if len(fs.received) != 4 {
		t.Fatalf("unexpected commands sent: %q", fs.received)
	}
}

func TestNormalizeData(t *testing.T) {
	ok := map[string]string{
		"Example.com":          "example.com",
		" 1.2.3.4 ":            "1.2.3.4",
		"2001:db8::1":          "2001:db8::1",
		"a.b  c@d.e   f@g.h":   "a.b c@d.e f@g.h",
		"mail.example.co.uk":   "mail.example.co.uk",
		"192.0.2.0/24":         "192.0.2.0/24",
		"1.2.3.4 <> rcpt@x.de": "1.2.3.4 <> rcpt@x.de",
	}
	for in, want := range ok {
		got, err := NormalizeData(in)
		if err != nil || got != want {
			t.Errorf("NormalizeData(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "-x", "--white foo", "a;b", "a\x00b", "ä.de"} {
		if _, err := NormalizeData(bad); err == nil {
			t.Errorf("NormalizeData(%q) accepted", bad)
		}
	}
}
