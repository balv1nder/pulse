package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/balv1nder/pulse/internal/config"
	"github.com/balv1nder/pulse/internal/limiter"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

var hostname, _ = os.Hostname()
var port string

// w is what you use to send responses back to the browser/client
// r is the incoming request, has .Method, .URL, .Header, .Body

type Item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := json.NewEncoder(w).Encode(v)
	if err != nil {
		log.Printf("Failed to encode JSON: %v", err)
	}
}

func getItem(rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		key := "item:" + id

		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()

		body, err := rdb.Get(ctx, key).Bytes()
		switch {
		case err == nil:
			w.Header().Set("X-Cache", "HIT")
		case errors.Is(err, redis.Nil):
			item := slowLookup(id)
			body, err = json.Marshal(item)
			if err != nil {
				log.Printf("failed to marshal item: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			rdb.Set(ctx, key, body, 30*time.Second)
			w.Header().Set("X-Cache", "MISS")
		default:
			item := slowLookup(id)
			body, err = json.Marshal(item)
			log.Printf("Redis error: %v", err)

		}

		w.Header().Set("X-Served-By", hostname+":"+port)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}

func slowLookup(id string) Item {
	time.Sleep(5 * time.Millisecond)
	return Item{
		ID:   id,
		Name: "Item-" + id,
	}
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func logging(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rec := &statusRecorder{
			ResponseWriter: w,
			code:           http.StatusOK,
		}

		next.ServeHTTP(rec, r) //next here is actually mux

		config.ReqDuration.WithLabelValues(
			r.Pattern,
			strconv.Itoa(rec.code),
		).Observe(float64(time.Since(start).Seconds()))

		log.Printf("%s %s %d %v", r.Method, r.URL.Path, rec.code, time.Since(start))
	})
}

func healthz(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "ok")
}

func rateLimit(l *limiter.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TODO: client from X-Client-ID, default "anonymous"
		client := r.Header.Get("X-Client-ID")
		if client == "" {
			client = "anonymous"
		}
		// TODO: ok, err := l.Allow(r.Context(), client)
		ok, err := l.Allow(r.Context(), client)
		// TODO: err → your policy; !ok → http.Error(w, "rate limited", http.StatusTooManyRequests) and return
		if err != nil {
			// Fail closed: if Redis is unavailable, do not bypass the rate limit.
			log.Printf("rate limiter error: %v", err)
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}

		if !ok {
			http.Error(w, "Rate Limit Reached", http.StatusTooManyRequests)
			return
		}
		// TODO: otherwise call next
		next.ServeHTTP(w, r)
	})
}

func main() {
	rdb := redis.NewClient(
		&redis.Options{
			Addr: config.Getenv("REDIS_ADDR", "localhost:6379"),
		},
	)
	redisCtx, cancel := context.WithTimeout(
		context.Background(),
		2*time.Second,
	)
	defer cancel()

	if err := rdb.Ping(redisCtx).Err(); err != nil {
		log.Fatal("Redis connection timeout!")
	}

	mux := http.NewServeMux()
	lim := limiter.New(rdb, 1000, time.Second)
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /items/{id}", rateLimit(lim, getItem(rdb)))

	port = config.Getenv("PORT", "8080")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      logging(mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("api listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	log.Println("bye")
}
