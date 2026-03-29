// Package transport provides a shared, tuned HTTP transport for all archive
// providers and the downloader.  A single shared transport means TCP+TLS
// connections to each host are pooled across every goroutine, eliminating
// handshake overhead on repeated requests to the same endpoint.
package transport

import (
	"net/http"
	"time"
)

// New returns a tuned *http.Transport ready for high-concurrency archive
// fetching.  Keep-alive, large connection pools, and HTTP/2 are enabled.
func New() *http.Transport {
	return &http.Transport{
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   30,
		MaxConnsPerHost:       50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}
