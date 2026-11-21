// Package greylistd implements a small client for the greylistd(8) UNIX socket
// protocol.
//
// The protocol is a single request/response exchange per connection: the
// client writes one command line (for example "add --white example.com"),
// half-closes its side of the connection and reads the reply until greylistd
// closes the socket. Error replies start with "error: ".
package greylistd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// List is one of greylistd's three data lists.
type List string

const (
	White List = "white"
	Grey  List = "grey"
	Black List = "black"
)

// Lists is the canonical display order of the lists.
var Lists = []List{White, Grey, Black}

// ParseList converts user input into a List.
func ParseList(s string) (List, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "white", "whitelist":
		return White, nil
	case "grey", "gray", "greylist", "graylist":
		return Grey, nil
	case "black", "blacklist":
		return Black, nil
	}
	return "", fmt.Errorf("unknown list %q", s)
}

// Entry is one item of a greylistd list as reported by the "list" command.
type Entry struct {
	List     List
	Data     string
	LastSeen time.Time
	Count    int
}

// Error is an error reply from greylistd ("error: ...").
type Error struct {
	Command string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("greylistd: %s: %s", e.Command, e.Message)
}

// ErrNotFound is returned by Delete when greylistd does not know the item.
var ErrNotFound = errors.New("greylistd: not found")

// Client talks to greylistd over its UNIX socket.
type Client struct {
	// SocketPath is the path of greylistd's UNIX socket.
	SocketPath string
	// Timeout bounds a single command round trip.
	Timeout time.Duration
	// Location is the time zone greylistd formats "Last Seen" timestamps in
	// (greylistd uses the local time of its host).
	Location *time.Location
}

// New returns a Client with sensible defaults.
func New(socketPath string) *Client {
	return &Client{SocketPath: socketPath, Timeout: 10 * time.Second, Location: time.Local}
}

// dataRe restricts what can be sent as list data. greylistd splits commands
// on whitespace, so data may consist of several words (a triplet); it must not
// start with "-" (that would be parsed as an option) and must not contain
// control characters.
var dataRe = regexp.MustCompile(`^[a-z0-9._:@+=/\[\]-]+( [a-z0-9._:@+=/<>\[\]-]+)*$`)

// NormalizeData lower-cases and validates a data item (domain, IP address or
// triplet). greylistd itself lower-cases everything it receives, so doing it
// here keeps what the UI shows identical to what greylistd stores.
func NormalizeData(s string) (string, error) {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	switch {
	case s == "":
		return "", errors.New("empty value")
	case len(s) > 512:
		return "", errors.New("value is too long")
	case strings.HasPrefix(s, "-"):
		return "", errors.New("must not start with '-'")
	case !dataRe.MatchString(s):
		return "", errors.New("contains invalid characters")
	}
	return s, nil
}

// Do sends a raw command and returns greylistd's reply. Error replies are
// returned as *Error.
func (c *Client) Do(ctx context.Context, command string) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return "", fmt.Errorf("greylistd: connect: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	if _, err := io.WriteString(conn, command); err != nil {
		return "", fmt.Errorf("greylistd: write: %w", err)
	}
	// Half-close so greylistd sees the end of the request even if it reads
	// until EOF.
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	reply, err := io.ReadAll(io.LimitReader(conn, 64<<20))
	if err != nil {
		return "", fmt.Errorf("greylistd: read: %w", err)
	}
	out := string(reply)
	if msg, ok := strings.CutPrefix(out, "error:"); ok {
		verb, _, _ := strings.Cut(command, " ")
		return "", &Error{Command: verb, Message: strings.TrimSpace(msg)}
	}
	return out, nil
}

// Ping checks that greylistd answers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Do(ctx, "status ping.invalid")
	return err
}

// List returns all entries of the given lists (all lists if none are given).
func (c *Client) List(ctx context.Context, lists ...List) ([]Entry, error) {
	cmd := "list"
	for _, l := range lists {
		cmd += " --" + string(l)
	}
	out, err := c.Do(ctx, cmd)
	if err != nil {
		var ge *Error
		// A fresh greylistd without any saved triplets reports a missing
		// triplet file; that simply means there is nothing to list.
		if errors.As(err, &ge) && strings.Contains(ge.Message, "No such file or directory") {
			return nil, nil
		}
		return nil, err
	}
	loc := c.Location
	if loc == nil {
		loc = time.Local
	}
	return parseList(out, loc)
}

// parseList parses the human-readable output of greylistd's "list" command:
//
//	Whitelist data:
//	===============
//	  Last Seen            Count  Data
//	  2026-11-21 09:56:52      1  example.com
func parseList(out string, loc *time.Location) ([]Entry, error) {
	var entries []Entry
	var current List
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasSuffix(trimmed, "list data:"):
			l, err := ParseList(strings.TrimSuffix(trimmed, " data:"))
			if err != nil {
				return nil, fmt.Errorf("greylistd: unexpected list header %q", trimmed)
			}
			current = l
			continue
		case strings.Trim(trimmed, "=") == "":
			continue
		case strings.HasPrefix(trimmed, "Last Seen"):
			continue
		}
		if current == "" {
			return nil, fmt.Errorf("greylistd: unexpected line %q", trimmed)
		}
		// "YYYY-MM-DD HH:MM:SS <count> <data...>"
		f := strings.Fields(trimmed)
		if len(f) < 4 {
			return nil, fmt.Errorf("greylistd: malformed entry %q", trimmed)
		}
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", f[0]+" "+f[1], loc)
		if err != nil {
			return nil, fmt.Errorf("greylistd: malformed timestamp in %q: %w", trimmed, err)
		}
		n, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("greylistd: malformed count in %q: %w", trimmed, err)
		}
		entries = append(entries, Entry{List: current, LastSeen: ts, Count: n, Data: strings.Join(f[3:], " ")})
	}
	return entries, sc.Err()
}

// Add puts data on the given list, moving it from another list if needed.
func (c *Client) Add(ctx context.Context, list List, data string) error {
	data, err := NormalizeData(data)
	if err != nil {
		return err
	}
	if _, err := ParseList(string(list)); err != nil {
		return err
	}
	_, err = c.Do(ctx, fmt.Sprintf("add --%s %s", list, data))
	return err
}

// Delete removes data from whatever list it is on.
func (c *Client) Delete(ctx context.Context, data string) error {
	data, err := NormalizeData(data)
	if err != nil {
		return err
	}
	_, err = c.Do(ctx, "delete "+data)
	var ge *Error
	if errors.As(err, &ge) && ge.Message == "Not found" {
		return ErrNotFound
	}
	return err
}

// Status returns the list data is on, or "unseen".
func (c *Client) Status(ctx context.Context, data string) (string, error) {
	data, err := NormalizeData(data)
	if err != nil {
		return "", err
	}
	out, err := c.Do(ctx, "status "+data)
	return strings.TrimSpace(out), err
}

// Stats returns greylistd's human-readable statistics.
func (c *Client) Stats(ctx context.Context) (string, error) {
	return c.Do(ctx, "stats")
}
