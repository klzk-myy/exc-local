package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpCHQuerier implements persistence.CHQuerier over the ClickHouse HTTP
// interface (port 8123): POST the SQL body, read the result stream. No
// clickhouse-go dependency is needed because the exporter only pipes raw
// CSV — it never decodes rows.
type httpCHQuerier struct {
	Base     string // e.g. http://127.0.0.1:8123
	User     string
	Password string
	Database string
}

func (c *httpCHQuerier) QueryCSV(ctx context.Context, query string) (io.ReadCloser, error) {
	u, err := url.Parse(strings.TrimRight(c.Base, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("clickhouse url: %w", err)
	}
	if c.Database != "" {
		q := u.Query()
		q.Set("database", c.Database)
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(),
		strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	client := &http.Client{Timeout: 10 * time.Minute} // full-day scans can be slow
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("clickhouse HTTP %d: %s", resp.StatusCode,
			strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}
