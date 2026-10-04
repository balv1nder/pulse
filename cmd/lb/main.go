package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/balv1nder/pulse/internal/config"
)

type backend struct {
	url   *url.URL
	proxy *httputil.ReverseProxy
	alive atomic.Bool
}

type pool struct {
	backends []*backend
	next     atomic.Uint64
}

func (p *pool) pick() *backend {
	n := uint64(len(p.backends))
	for i := uint64(0); i < n; i++ {
		next := p.backends[p.next.Add(1)%uint64(len(p.backends))]
		if next.alive.Load() {
			return next
		}
	}
	return nil
}

func (p *pool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := p.pick()

	if b == nil {
		http.Error(w, "Service Unavailable!", http.StatusServiceUnavailable)
		return
	}

	b.proxy.ServeHTTP(w, r)
}

func (p *pool) healthCheck(every time.Duration) {
	client := &http.Client{Timeout: time.Second}
	for range time.Tick(every) {
		for _, b := range p.backends {
			resp, err := client.Get(b.url.String() + "/healthz")
			if resp != nil {
				resp.Body.Close()
			}
			ok := err == nil && resp != nil && resp.StatusCode == http.StatusOK
			if ok != b.alive.Load() {
				log.Printf("backend %s alive=%t", b.url, ok)
			}
			b.alive.Store(ok)
		}
	}
}

func main() {
	rawBackends := config.Getenv("BACKENDS", "http://localhost:8080,http://localhost:8081,http://localhost:8082")
	if rawBackends == "" {
		log.Fatalf("BACKENDS not configured!")
	}

	p := &pool{}

	for _, rawURL := range strings.Split(rawBackends, ",") {
		u, err := url.Parse(rawURL)
		if err != nil {
			log.Fatalf("invalid URL: %q, %v", rawURL, err)
		}
		b := &backend{
			url:   u,
			proxy: httputil.NewSingleHostReverseProxy(u),
			alive: atomic.Bool{},
		}
		b.alive.Store(true)

		b.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("backend %s failed: %v", b.url, err)
			b.alive.Store(false)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		}

		p.backends = append(p.backends, b)
	}
	go p.healthCheck(2 * time.Second)
	log.Fatal(http.ListenAndServe(":8000", p))
}
