package limiter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestAllowIsExactUnderConcurrency(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	l := New(rdb, 1000, 10*time.Second)
	client := fmt.Sprintf("test-%d", time.Now().UnixNano())

	var allowed atomic.Int64
	var wg sync.WaitGroup

	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// TODO: 30 times: call l.Allow; on error t.Error it; if ok, allowed.Add(1)
			for i := 0; i < 30; i++ {
				ok, err := l.Allow(context.Background(), client)

				if err != nil {
					t.Error(err)
					return
				}

				if ok {
					allowed.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	// TODO: if allowed.Load() != 1000, t.Fatalf with the actual number
	if allowed.Load() != 1000 {
		t.Fatalf("expected exactly 1000 allowed requests, got %d", allowed.Load())
	}
}
