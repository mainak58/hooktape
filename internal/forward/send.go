package forward

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mainak58/hooktape/internal/model"
)

const maxResponseBody = 64 << 10

var skippedHeaders = map[string]bool{
	"Host":                true,
	"Content-Length":      true,
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Connection":    true,
	"Transfer-Encoding":   true,
	"Te":                  true,
	"Trailer":             true,
	"Upgrade":             true,
	"Proxy-Authorization": true,
	"Proxy-Authenticate":  true,
	"Accept-Encoding":     true,
	"X-Hooktape-Id":       true,
	"X-Hooktape-Replay":   true,
}

type Result struct {
	Status   int // 0 if request failed before a response
	Duration time.Duration
	Body     []byte
	Err      error
}

type SendOptions struct {
	Replay bool // adds X-Hooktape-Replay: true
}

func Send(ctx context.Context, client *http.Client, r *model.Request, target string, opts SendOptions) Result {
	url := strings.TrimRight(target, "/") + r.Path
	if r.Query != "" {
		url += "?" + r.Query
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, url, bytes.NewReader(r.Body))
	if err != nil {
		return Result{Err: err}
	}

	for name, values := range r.Headers {
		name = http.CanonicalHeaderKey(name)
		if skippedHeaders[name] || strings.HasPrefix(name, "Cf-") {
			continue
		}
		req.Header[name] = values
	}

	req.Header.Set("X-Hooktape-Id", strconv.FormatInt(r.ID, 10))
	if opts.Replay {
		req.Header.Set("X-Hooktape-Replay", "true")
	}

	start := time.Now()

	res, err := client.Do(req)
	if err != nil {
		return Result{Duration: time.Since(start), Err: err}
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBody))

	return Result{Status: res.StatusCode, Duration: time.Since(start), Body: body, Err: err}
}

func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
