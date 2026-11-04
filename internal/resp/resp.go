// Package resp is a small Redis client speaking RESP2 over TCP. It covers
// what Recura needs (commands, pipelining-free request/response, a
// connection pool, AUTH and SELECT) without an external dependency.
package resp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Value is a decoded RESP reply.
type Value struct {
	Kind  byte // '+', '-', ':', '$', '*'
	Str   string
	Int   int64
	Array []Value
	Null  bool
}

// Err is a Redis error reply.
type Err string

func (e Err) Error() string { return "redis: " + string(e) }

// ErrNil is returned by helpers when the reply is a null bulk string.
var ErrNil = errors.New("redis: nil")

// Client is a pooled Redis client. It is safe for concurrent use.
type Client struct {
	addr     string
	password string
	username string
	db       int
	timeout  time.Duration
	pool     chan *conn
	mu       sync.Mutex
	closed   bool
}

type conn struct {
	c net.Conn
	r *bufio.Reader
	w *bufio.Writer
}

// Dial parses a redis:// URL (redis://[user:pass@]host:port[/db]) and
// returns a client. It verifies connectivity with PING.
func Dial(ctx context.Context, rawURL string, poolSize int) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	if u.Scheme != "redis" {
		return nil, fmt.Errorf("redis url: unsupported scheme %q (use redis://)", u.Scheme)
	}
	c := &Client{addr: u.Host, timeout: 5 * time.Second}
	if !strings.Contains(c.addr, ":") {
		c.addr += ":6379"
	}
	if u.User != nil {
		c.username = u.User.Username()
		c.password, _ = u.User.Password()
		if c.password == "" && c.username != "" {
			c.password, c.username = c.username, ""
		}
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		if c.db, err = strconv.Atoi(p); err != nil {
			return nil, fmt.Errorf("redis url: bad db %q", p)
		}
	}
	if poolSize <= 0 {
		poolSize = 16
	}
	c.pool = make(chan *conn, poolSize)
	if _, err := c.Do(ctx, "PING"); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) dial(ctx context.Context) (*conn, error) {
	d := net.Dialer{Timeout: c.timeout}
	nc, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, err
	}
	cn := &conn{c: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}
	if c.password != "" {
		args := []string{"AUTH", c.password}
		if c.username != "" {
			args = []string{"AUTH", c.username, c.password}
		}
		if _, err := cn.do(ctx, c.timeout, args...); err != nil {
			nc.Close()
			return nil, err
		}
	}
	if c.db != 0 {
		if _, err := cn.do(ctx, c.timeout, "SELECT", strconv.Itoa(c.db)); err != nil {
			nc.Close()
			return nil, err
		}
	}
	return cn, nil
}

func (c *Client) get(ctx context.Context) (*conn, error) {
	select {
	case cn := <-c.pool:
		return cn, nil
	default:
		return c.dial(ctx)
	}
}

func (c *Client) put(cn *conn) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		cn.c.Close()
		return
	}
	select {
	case c.pool <- cn:
	default:
		cn.c.Close()
	}
}

// Do sends one command and returns its reply. A Redis error reply is
// returned as an Err.
func (c *Client) Do(ctx context.Context, args ...string) (Value, error) {
	cn, err := c.get(ctx)
	if err != nil {
		return Value{}, err
	}
	v, err := cn.do(ctx, c.timeout, args...)
	var rerr Err
	if err != nil && !errors.As(err, &rerr) {
		cn.c.Close() // connection state unknown; drop it
		return Value{}, err
	}
	c.put(cn)
	return v, err
}

// Close closes pooled connections.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	for {
		select {
		case cn := <-c.pool:
			cn.c.Close()
		default:
			return nil
		}
	}
}

func (cn *conn) do(ctx context.Context, timeout time.Duration, args ...string) (Value, error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = cn.c.SetDeadline(deadline)
	fmt.Fprintf(cn.w, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(cn.w, "$%d\r\n%s\r\n", len(a), a)
	}
	if err := cn.w.Flush(); err != nil {
		return Value{}, err
	}
	v, err := read(cn.r)
	if err != nil {
		return Value{}, err
	}
	if v.Kind == '-' {
		return v, Err(v.Str)
	}
	return v, nil
}

func read(r *bufio.Reader) (Value, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return Value{}, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return Value{}, errors.New("redis: malformed reply")
	}
	kind, body := line[0], line[1:len(line)-2]
	switch kind {
	case '+', '-':
		return Value{Kind: kind, Str: body}, nil
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		return Value{Kind: kind, Int: n}, err
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return Value{}, err
		}
		if n < 0 {
			return Value{Kind: kind, Null: true}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return Value{}, err
		}
		return Value{Kind: kind, Str: string(buf[:n])}, nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil {
			return Value{}, err
		}
		if n < 0 {
			return Value{Kind: kind, Null: true}, nil
		}
		arr := make([]Value, n)
		for i := range arr {
			if arr[i], err = read(r); err != nil {
				return Value{}, err
			}
		}
		return Value{Kind: kind, Array: arr}, nil
	}
	return Value{}, fmt.Errorf("redis: unknown reply type %q", kind)
}

// Strings flattens an array reply of bulk strings. Null elements become "".
func (v Value) Strings() []string {
	out := make([]string, len(v.Array))
	for i, x := range v.Array {
		out[i] = x.Str
	}
	return out
}
