package config

import (
	"log"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

func Getenv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Printf("%s config not present, defaulting to %s", key, fallback)
		value = fallback
	}
	return value
}

var ReqDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "http_request_duration_seconds",
	Help:    "Request latency by route and status code",
	Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14),
}, []string{"route", "code"})
