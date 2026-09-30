// stub_server.go — Go counterpart of stub_server.py for the HAProxy
// validation drill (Task 5.3.29). Same contract:
//
//	stub_server <COLOR> <PORT>
//	GET /health/ready|/health/live -> 200 "<COLOR>\n" (option httpchk gate)
//	GET/POST any path             -> 200 "<COLOR>\n" + X-Stub-Color
//	Upgrade: websocket            -> honest 101 + Sec-WebSocket-Accept,
//	                               then close (no frame codec)
//
// Used to prove the blue/green map-flip works against a real Go service
// backend — the same runtime class as the order gateway it stands in for.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func main() {
	color := "UNKNOWN"
	port := "18081"
	if len(os.Args) > 1 {
		color = os.Args[1]
	}
	if len(os.Args) > 2 {
		port = os.Args[2]
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			// Honest 101 — hijack after upgrading, then close: HAProxy
			// only needs the 101 to enter tunnel mode.
			key := r.Header.Get("Sec-WebSocket-Key")
			sum := sha1.Sum([]byte(key + wsGUID))
			accept := base64.StdEncoding.EncodeToString(sum[:])
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", 500)
				return
			}
			conn, bufrw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprintf(bufrw, "HTTP/1.1 101 Switching Protocols\r\n"+
				"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
				"Sec-WebSocket-Accept: %s\r\nX-Stub-Color: %s\r\n\r\n",
				accept, color)
			bufrw.Flush()
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Stub-Color", color)
		fmt.Fprintf(w, "%s\n", color)
	})

	fmt.Fprintf(os.Stderr, "stub %s listening on 0.0.0.0:%s\n", color, port)
	if err := http.ListenAndServe("0.0.0.0:"+port, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
