package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrRedisNil = errors.New("redis: nil")
)

// RedisClient is a lightweight, zero-dependency RESP (REdis Serialization Protocol) client.
type RedisClient struct {
	addr     string
	password string
	mu       sync.Mutex
	conn     net.Conn
	reader   *bufio.Reader
}

func NewRedisClient(addr, password string) *RedisClient {
	return &RedisClient{
		addr:     addr,
		password: password,
	}
}

func (c *RedisClient) connect() error {
	if c.conn != nil {
		return nil
	}
	conn, err := net.DialTimeout("tcp", c.addr, 3*time.Second)
	if err != nil {
		return err
	}
	c.conn = conn
	c.reader = bufio.NewReader(conn)

	if c.password != "" {
		if err := c.auth(); err != nil {
			_ = conn.Close()
			c.conn = nil
			return err
		}
	}
	return nil
}

func (c *RedisClient) auth() error {
	cmd := fmt.Sprintf("*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(c.password), c.password)
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return err
	}
	resp, err := c.reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(resp, "+OK") {
		return fmt.Errorf("redis auth failed: %s", strings.TrimSpace(resp))
	}
	return nil
}

func (c *RedisClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// Ping checks server connectivity.
func (c *RedisClient) Ping(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.connect(); err != nil {
		return err
	}
	if _, err := c.conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		c.conn = nil
		return err
	}
	resp, err := c.reader.ReadString('\n')
	if err != nil {
		c.conn = nil
		return err
	}
	if !strings.HasPrefix(resp, "+PONG") {
		return fmt.Errorf("unexpected ping response: %s", resp)
	}
	return nil
}

// Set stores a key with expiration in milliseconds (PX).
func (c *RedisClient) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.connect(); err != nil {
		return err
	}

	ttlMs := ttl.Milliseconds()
	if ttlMs <= 0 {
		ttlMs = 86400000 // default 24h
	}
	ttlStr := strconv.FormatInt(ttlMs, 10)

	// Format: *5\r\n$3\r\nSET\r\n$<klen>\r\n<key>\r\n$<vlen>\r\n<val>\r\n$2\r\nPX\r\n$<tlen>\r\n<ttl>\r\n
	var buf bytes.Buffer
	buf.WriteString("*5\r\n$3\r\nSET\r\n")
	buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(key), key))
	buf.WriteString(fmt.Sprintf("$%d\r\n", len(val)))
	buf.Write(val)
	buf.WriteString("\r\n$2\r\nPX\r\n")
	buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(ttlStr), ttlStr))

	if _, err := c.conn.Write(buf.Bytes()); err != nil {
		c.conn = nil
		return err
	}

	line, err := c.reader.ReadString('\n')
	if err != nil {
		c.conn = nil
		return err
	}
	if !strings.HasPrefix(line, "+OK") {
		return fmt.Errorf("redis set error: %s", line)
	}
	return nil
}

// Get retrieves a key's bytes. Returns ErrRedisNil if key does not exist.
func (c *RedisClient) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.connect(); err != nil {
		return nil, err
	}

	cmd := fmt.Sprintf("*2\r\n$3\r\nGET\r\n$%d\r\n%s\r\n", len(key), key)
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		c.conn = nil
		return nil, err
	}

	prefixByte, err := c.reader.ReadByte()
	if err != nil {
		c.conn = nil
		return nil, err
	}

	if prefixByte == '-' {
		line, _ := c.reader.ReadString('\n')
		return nil, fmt.Errorf("redis error: %s", line)
	}

	if prefixByte != '$' {
		line, _ := c.reader.ReadString('\n')
		return nil, fmt.Errorf("unexpected redis get reply prefix: %c, line: %s", prefixByte, line)
	}

	lenStr, err := c.reader.ReadString('\n')
	if err != nil {
		c.conn = nil
		return nil, err
	}
	length, err := strconv.Atoi(strings.TrimSpace(lenStr))
	if err != nil {
		c.conn = nil
		return nil, err
	}

	if length == -1 {
		return nil, ErrRedisNil
	}

	valBuf := make([]byte, length)
	if _, err := io.ReadFull(c.reader, valBuf); err != nil {
		c.conn = nil
		return nil, err
	}
	// read trailing \r\n
	_, _ = c.reader.ReadByte()
	_, _ = c.reader.ReadByte()

	return valBuf, nil
}

// ----------------------------------------------------------------------------
// RedisIdempotencyStore
// ----------------------------------------------------------------------------

type RedisIdempotencyStore struct {
	client *RedisClient
}

func NewRedisIdempotencyStore(client *RedisClient) *RedisIdempotencyStore {
	return &RedisIdempotencyStore{client: client}
}

func (s *RedisIdempotencyStore) Get(ctx context.Context, key string) (int, []byte, bool, error) {
	raw, err := s.client.Get(ctx, "idemp:"+key)
	if err != nil {
		if errors.Is(err, ErrRedisNil) {
			return 0, nil, false, nil
		}
		return 0, nil, false, err
	}

	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return 0, nil, false, errors.New("malformed redis idempotency payload")
	}

	code, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, nil, false, err
	}

	body, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, nil, false, err
	}

	return code, body, true, nil
}

func (s *RedisIdempotencyStore) Set(ctx context.Context, key string, statusCode int, body []byte, ttl time.Duration) error {
	encodedBody := base64.StdEncoding.EncodeToString(body)
	val := fmt.Sprintf("%d|%s", statusCode, encodedBody)
	return s.client.Set(ctx, "idemp:"+key, []byte(val), ttl)
}
