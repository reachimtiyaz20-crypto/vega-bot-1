package orderbook

import (
	"context"
	"io"
	"net/http"
	"time"
)

// clientShim is a minimal GET helper so venue readers do not each repeat the
// same request/limit/read boilerplate.
type clientShim struct {
	HTTP *http.Client
}

func newShim(timeout time.Duration) *clientShim {
	return &clientShim{HTTP: &http.Client{Timeout: timeout}}
}

func (c *clientShim) get(ctx context.Context, url string) ([]byte, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snip := raw
		if len(snip) > 200 {
			snip = snip[:200]
		}
		return nil, &httpError{Code: resp.StatusCode, Body: string(snip)}
	}
	return raw, nil
}

type httpError struct {
	Code int
	Body string
}

func (e *httpError) Error() string {
	return "HTTP " + itoa(e.Code) + ": " + e.Body
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
