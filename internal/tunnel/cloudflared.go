// Package tunnel runs cloudflared as a child process so webhooks from the
// public internet reach the local catcher.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// startTimeout is how long cloudflared gets to report a working tunnel. It is a
// variable so tests do not have to wait it out.
var startTimeout = 30 * time.Second

// registeredLine is what cloudflared logs once an edge connection is live.
const registeredLine = "Registered tunnel connection"

// quickURL matches the hostname cloudflared prints for a quick tunnel.
var quickURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

type Options struct {
	LocalURL   string // http://127.0.0.1:8080
	TunnelName string // "" means a quick tunnel
	Binary     string // defaults to "cloudflared", found on PATH
	Logf       func(format string, args ...any)
}

type Tunnel struct {
	PublicURL string // "" for a named tunnel, whose hostname lives in Cloudflare's config

	cmd    *exec.Cmd
	exited chan struct{}
}

// Start launches cloudflared and returns once it reports a usable tunnel. The
// returned channel carries the exit error if cloudflared later dies on its own.
func Start(ctx context.Context, o Options) (*Tunnel, <-chan error, error) {
	binary := o.Binary
	if binary == "" {
		binary = "cloudflared"
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, nil, err
	}

	args := []string{"tunnel", "--no-autoupdate"}
	if o.TunnelName != "" {
		args = append(args, "run", "--url", o.LocalURL, o.TunnelName)
	} else {
		args = append(args, "--url", o.LocalURL)
	}

	cmd := exec.Command(path, args...)
	setProcessGroup(cmd)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	t := &Tunnel{cmd: cmd, exited: make(chan struct{})}
	ready := make(chan string, 1)
	exit := make(chan error, 1)

	// cmd.Wait closes the stderr pipe, so it may only run once the reader has
	// seen EOF.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		readLines(stderr, o, ready)
	}()
	go func() {
		<-drained
		exit <- cmd.Wait()
		close(t.exited)
	}()

	select {
	case url := <-ready:
		t.PublicURL = url
		return t, exit, nil
	case err := <-exit:
		return nil, nil, fmt.Errorf("cloudflared exited during startup: %w", err)
	case <-ctx.Done():
		t.Stop()
		return nil, nil, ctx.Err()
	case <-time.After(startTimeout):
		t.Stop()
		return nil, nil, fmt.Errorf("cloudflared reported no tunnel within %s", startTimeout)
	}
}

// Stop ends cloudflared, politely first and then not.
func (t *Tunnel) Stop() error {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return nil
	}

	select {
	case <-t.exited:
		return nil
	default:
	}

	if err := terminate(t.cmd); err != nil && !errors.Is(err, exec.ErrNotFound) {
		return err
	}

	select {
	case <-t.exited:
		return nil
	case <-time.After(3 * time.Second):
	}

	if err := kill(t.cmd); err != nil {
		return err
	}
	<-t.exited

	return nil
}

// readLines watches cloudflared's stderr for the one line that says the tunnel
// is up, then keeps draining so the child never blocks on a full pipe.
func readLines(r io.Reader, o Options, ready chan<- string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20) // cloudflared logs long JSON lines

	// A quick tunnel prints its URL before the edge connection exists, and
	// cloudflared itself warns the hostname "may take some time to be
	// reachable", so both lines have to arrive before the URL is worth handing
	// out. Order between them is not guaranteed.
	var url string
	registered, reported := false, false

	for scanner.Scan() {
		line := scanner.Text()

		if o.Logf != nil {
			o.Logf("[cloudflared] %s", line)
		}
		if reported {
			continue
		}

		if strings.Contains(line, registeredLine) {
			registered = true
		}
		if o.TunnelName == "" && url == "" {
			if found, ok := parseQuickURL(line); ok {
				url = found
			}
		}

		if registered && (o.TunnelName != "" || url != "") {
			reported = true
			ready <- url
		}
	}
}

// parseQuickURL pulls the public URL out of one line of cloudflared output.
func parseQuickURL(line string) (string, bool) {
	match := quickURL.FindString(line)

	return match, match != ""
}
