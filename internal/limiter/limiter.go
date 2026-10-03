package limiter

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// KEYS[1] = counter key, ARGV[1] = window in ms. Returns the new count.
// If first request, PEXPIRE counter, return count (dont extend PEXPIRE on all req)
// n==1 because the first request creates the key
var script = redis.NewScript(`
	local n = redis.call("INCR", KEYS[1])
	if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
	return n
`)

type Limiter struct {
	// TODO: rdb *redis.Client, limit int64, window time.Duration
	rdb    *redis.Client
	limit  int64
	window time.Duration
}

func New(rdb *redis.Client, limit int64, window time.Duration) *Limiter {
	return &Limiter{
		rdb:    rdb,
		limit:  limit,
		window: window,
	}
}

// Allow reports whether this client may make one more request right now.
func (l *Limiter) Allow(ctx context.Context, client string) (bool, error) {
	// TODO 1: window number = time.Now().UnixMilli() / l.window.Milliseconds()
	windowNumber := time.Now().UnixMilli() / l.window.Milliseconds()
	// TODO 2: key with fmt.Sprintf("rl:%s:%d", ...)
	key := fmt.Sprintf("rl:%s:%d", client, windowNumber)
	// TODO 3: n, err := script.Run(ctx, l.rdb, []string{key}, l.window.Milliseconds()).Int64()
	n, err := script.Run(
		ctx,
		l.rdb,
		[]string{key},
		l.window.Milliseconds(),
	).Int64()

	if err != nil {
		return false, err
	}
	//Run the lua script (Redis client, counter key, expiry duration), return counter val

	// TODO 4: return n <= l.limit (and pass errors up; don't decide policy here)
	return n <= l.limit, nil
}
