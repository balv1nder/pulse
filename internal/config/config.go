package config

import (
	"log"
	"os"
)

func Getenv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Printf("%s config not present, defaulting to %s", key, fallback)
		value = fallback
	}
	return value
}
