package balancer

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

// hcClient is the HTTP client used for health probes.
type hcClient struct {
	transport http.RoundTripper
}

// newHCClient creates an HTTP client tuned for health probes.
func newHCClient(cfg *config.Config) *hcClient {
	tc := cfg.Server.Transport
	return &hcClient{
		transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConns:        tc.MaxIdleConns,
			MaxIdleConnsPerHost: tc.MaxIdleConnsPerHost,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     time.Duration(tc.IdleConnTimeout) * time.Second,
		},
	}
}

func (c *hcClient) probe(url string, timeout time.Duration) (int, error) {
	client := &http.Client{
		Transport:     c.transport,
		Timeout:       timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	statusCode := resp.StatusCode
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return statusCode, nil
}
