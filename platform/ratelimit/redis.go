package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// callTimeout bounds one limiter call. The limiter sits in front of user
// requests, so a slow Redis must cost a request milliseconds, not seconds.
const callTimeout = 150 * time.Millisecond

// The Redis client writes its own unstructured lines to stderr when it
// cannot connect. They would break the services' JSON logs and repeat what
// Guard already reports, with context, for every request it lets through.
type discardLogger struct{}

func (discardLogger) Printf(context.Context, string, ...any) {}

func init() { redis.SetLogger(discardLogger{}) }

// fixedWindow counts one action and returns the count and the time left in
// the window. INCR and PEXPIRE run in one script so a crash between them
// cannot leave a counter that never expires.
var fixedWindow = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {n, ttl}
`)

// Redis is a fixed-window limiter backed by Redis.
//
// Fixed window: the first action opens a window of limit.Window; actions are
// counted until it expires. It is one round trip and one small key per
// subject. Its known weakness is that a subject can do up to 2x Max across a
// window boundary; for abuse protection that is acceptable, and the limits
// are set with it in mind.
type Redis struct {
	client *redis.Client
	prefix string
}

// NewRedis connects lazily: construction never fails because Redis is down.
// prefix separates services (and test runs) sharing one Redis.
func NewRedis(addr, password, prefix string) *Redis {
	return &Redis{
		prefix: prefix,
		client: redis.NewClient(&redis.Options{
			Addr: addr, Password: password,
			DialTimeout: callTimeout, ReadTimeout: callTimeout, WriteTimeout: callTimeout,
			// No client retries: a retry would double the delay added to a
			// user request, and the caller already has a policy for failure.
			MaxRetries: -1,
		}),
	}
}

func (r *Redis) Close() error { return r.client.Close() }

// Ping reports whether Redis is reachable; used by tests and readiness logs.
func (r *Redis) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

// key hashes the subject so identifiers such as phone numbers are not
// readable in Redis.
func (r *Redis) key(limit Limit, subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return fmt.Sprintf("%s:rl:%s:%s", r.prefix, limit.Name, hex.EncodeToString(sum[:12]))
}

func (r *Redis) Allow(ctx context.Context, limit Limit, subject string) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := fixedWindow.Run(ctx, r.client, []string{r.key(limit, subject)}, limit.Window.Milliseconds()).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("ratelimit: redis: %w", err)
	}
	if len(res) != 2 {
		return Decision{}, fmt.Errorf("ratelimit: unexpected script result %v", res)
	}
	return Decision{Allowed: res[0] <= int64(limit.Max), RetryAfter: time.Duration(res[1]) * time.Millisecond}, nil
}
