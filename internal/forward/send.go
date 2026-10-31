package forward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
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
	"Expect":              true,
	"X-Hooktape-Id":       true,
	"X-Hooktape-Replay":   true,
}

// ErrTruncated is why a body cut off at --max-body is never sent: the app would
// only see a corrupt payload and a failed signature.
var ErrTruncated = errors.New("body was truncated when caught, so it was not sent")

type Result struct {
	Status   int // 0 if request failed before a response
	Duration time.Duration
	Body     []byte
	Err      error
}

type SendOptions struct {
	Replay          bool // adds X-Hooktape-Replay: true
	KeepPath        bool // appends the path the caller sent; off, only target is reached
	KeepCredentials bool // keeps Authorization and Cookie on a replay to a non-local target
}

func Send(ctx context.Context, client *http.Client, r *model.Request, target string, opts SendOptions) Result {
	if r.Truncated {
		return Result{Err: ErrTruncated}
	}

	dest := target
	if opts.KeepPath {
		if escapesBase(r.Path) {
			return Result{Err: fmt.Errorf("path %q climbs above the target, so it was not sent", r.Path)}
		}
		dest = strings.TrimRight(target, "/") + r.Path
	}
	if r.Query != "" {
		if strings.Contains(dest, "?") {
			dest += "&" + r.Query
		} else {
			dest += "?" + r.Query
		}
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, dest, bytes.NewReader(r.Body))
	if err != nil {
		return Result{Err: err}
	}

	dropCredentials := opts.Replay && !opts.KeepCredentials && !IsLocalTarget(target)

	for name, values := range r.Headers {
		name = http.CanonicalHeaderKey(name)
		if skippedHeaders[name] || strings.HasPrefix(name, "Cf-") {
			continue
		}
		if dropCredentials && (name == "Authorization" || name == "Cookie") {
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

// NewLocalClient is NewClient for targets typed into the dashboard. The check
// runs on the address actually dialled, so a hostname that resolves or rebinds
// to a public IP is refused too. It skips any proxy for the same reason.
func NewLocalClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Control: func(network, address string, c syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || !isLocalIP(ip) {
			return fmt.Errorf("refusing to connect to %s: replay targets must be local (see --replay-any-host)", host)
		}

		return nil
	}}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = dialer.DialContext

	client := NewClient(timeout)
	client.Transport = transport

	return client
}

// CheckTarget validates a URL this process will fetch: it has to be an http(s)
// address and nothing else.
func CheckTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", errors.New("no target: set a replay target first")
	}

	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("invalid target: use http://host:port")
	}

	return target, nil
}

// IsLocalTarget reports whether target is this machine or a private network,
// going by the URL alone. A hostname other than localhost counts as remote.
func IsLocalTarget(target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Hostname(), "localhost") {
		return true
	}

	ip := net.ParseIP(u.Hostname())

	return ip != nil && isLocalIP(ip)
}

func isLocalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate()
}

// escapesBase looks for a ".." segment after decoding, since an app may decode
// %2e and %2f before it routes. Backslashes count for apps on Windows.
func escapesBase(path string) bool {
	decoded, err := url.PathUnescape(path)
	if err != nil {
		decoded = path
	}

	segments := strings.FieldsFunc(decoded, func(c rune) bool { return c == '/' || c == '\\' })
	for _, segment := range segments {
		if segment == ".." {
			return true
		}
	}

	return false
}
