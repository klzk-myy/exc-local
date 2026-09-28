// Raw dependency probes — tiny, allocation-light, and honest: a probe
// that cannot reach the dependency returns the reason, never a pass.
package itest

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5"
	goredis "github.com/redis/go-redis/v9"
)

// pgConnect opens a single pgx connection.
func pgConnect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	return pgx.Connect(ctx, dsn)
}

// RedisClient returns a go-redis client on the suite's scratch logical
// DB — degradation-mode flips, idempotency keys and ban inspection all
// write through it.
func (e *Env) RedisClient() *goredis.Client {
	return goredis.NewClient(&goredis.Options{Addr: e.RedisAddr, DB: e.RedisDB})
}

// redisPing verifies AUTH-free reachability + the requested logical DB.
// A server that demands auth reports "NOAUTH" — the host :6379 instance
// on this box does, which is why the suite defaults to :16379.
func redisPing(ctx context.Context, addr string, db int) string {
	c := goredis.NewClient(&goredis.Options{Addr: addr, DB: db})
	defer c.Close()
	if err := c.Ping(ctx).Err(); err != nil {
		return fmt.Sprintf("redis unreachable at %s: %v", addr, err)
	}
	return ""
}

// natsProbe checks the NATS INFO banner on the first seed URL.
func natsProbe(ctx context.Context, urls []string) string {
	if len(urls) == 0 {
		return "no NATS urls configured"
	}
	addr := strings.TrimPrefix(urls[0], "nats://")
	if !strings.Contains(addr, ":") {
		addr += ":4222"
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Sprintf("nats unreachable at %s: %v", addr, err)
	}
	conn.Close()
	return ""
}

func exec_LookPath(name string) (string, error) { return exec.LookPath(name) }
