// Package redis provides the go-redis client factory.
// The full key schema (sessions, rate limits, coordination keys) is
// Task 1.3.4 — this file is the minimal factory only.
package redis

import (
	goredis "github.com/redis/go-redis/v9"
)

// New returns a Redis client for the coordination instance.
// Callers should Ping at startup if connectivity must be verified.
func New(addr, password string, db int) *goredis.Client {
	return goredis.NewClient(&goredis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
}
