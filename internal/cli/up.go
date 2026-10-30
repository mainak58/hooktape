package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/mainak58/hooktape/internal/catcher"
	"github.com/mainak58/hooktape/internal/config"
	"github.com/mainak58/hooktape/internal/forward"
	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/render"
	"github.com/mainak58/hooktape/internal/store"
	"github.com/spf13/cobra"
)

// queueSize is the forward backlog. One worker drains it, so forwards reach the
// app in arrival order.
const queueSize = 256

var (
	upForward    string
	upPort       int
	upAddr       string
	upMaxBody    string
	upRespond    int
	upTimeout    time.Duration
	upNoTunnel   bool
	upTunnelName string
	upRetention  string
)

var upCmd = &cobra.Command{
	Use:     "up",
	Short:   "Catch webhooks and forward them to your app",
	Example: "  hooktape up --forward http://localhost:3000",
	Args:    cobra.NoArgs,
	RunE:    runUp,
}

func init() {
	flags := upCmd.Flags()
	flags.StringVar(&upForward, "forward", "", "base URL to forward to (empty means capture only)")
	flags.IntVar(&upPort, "port", 8080, "port to listen on")
	flags.StringVar(&upAddr, "addr", "127.0.0.1", "address to listen on")
	flags.StringVar(&upMaxBody, "max-body", "1MB", "largest body to store")
	flags.IntVar(&upRespond, "respond", 200, "status to return to the provider")
	flags.DurationVar(&upTimeout, "timeout", 10*time.Second, "timeout for each forward")
	flags.BoolVar(&upNoTunnel, "no-tunnel", false, "do not start a public tunnel")
	flags.StringVar(&upTunnelName, "tunnel-name", "", "name for the public tunnel")
	flags.StringVar(&upRetention, "retention", "0", "delete requests older than this (e.g. 7d; 0 keeps everything)")
	rootCmd.AddCommand(upCmd)
}

func runUp(cmd *cobra.Command, args []string) error {
	maxBody, err := humanize.ParseBytes(upMaxBody)
	if err != nil {
		return fmt.Errorf("invalid --max-body %q", upMaxBody)
	}

	retention, err := parseRetention(upRetention)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(upAddr, strconv.Itoa(upPort)))
	if err != nil {
		return err
	}

	cmd.SilenceUsage = true

	s, err := store.Open(dbPath)
	if err != nil {
		listener.Close()
		return err
	}
	defer s.Close()

	logf := log.New(cmd.OutOrStdout(), "", 0).Printf
	colors := paletteFor(cmd)

	var fwd *forwarder
	if upForward != "" {
		fwd = newForwarder(s, upForward, upTimeout, colors, logf)
		fwd.start()
		_ = config.Save(config.Config{LastForward: upForward})
	}

	handler := &catcher.Handler{
		Store:     s,
		Forwarder: fwd.orNil(),
		MaxBody:   int64(maxBody),
		Respond:   upRespond,
		Logf:      logf,
	}

	srv := &http.Server{
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if retention > 0 {
		startRetention(ctx, s, retention, logf)
	}

	printBanner(cmd, colors, listener.Addr().String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logf("")
		logf("shutting down")
	}

	// Shutdown first: it waits for handlers to return, so no further Forward
	// call can arrive once the queue is closed.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logf("shutdown: %v", err)
	}
	fwd.stop(5 * time.Second)

	return nil
}

func printBanner(cmd *cobra.Command, colors render.Palette, addr string) {
	out := cmd.OutOrStdout()

	target := upForward
	if target == "" {
		target = colors.Dim("capture only (no --forward)")
	}

	public := "(starting tunnel…)"
	if upNoTunnel {
		public = colors.Dim("off (--no-tunnel)")
	}

	fmt.Fprintln(out, colors.Bold("hooktape up"))
	fmt.Fprintf(out, "  db        %s\n", dbPath)
	fmt.Fprintf(out, "  catcher   http://%s/h/<endpoint>/...\n", addr)
	fmt.Fprintf(out, "  forward   %s\n", target)
	fmt.Fprintf(out, "  public    %s\n", public)
	fmt.Fprintln(out)
}

func parseRetention(value string) (time.Duration, error) {
	if value == "" || value == "0" {
		return 0, nil
	}

	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid --retention %q", value)
		}
		return time.Duration(n * 24 * float64(time.Hour)), nil
	}

	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid --retention %q", value)
	}

	return d, nil
}

func startRetention(ctx context.Context, s *store.Store, window time.Duration, logf func(string, ...any)) {
	prune := func() {
		deleted, err := s.DeleteOlderThan(ctx, time.Now().Add(-window).UnixMilli())
		switch {
		case err != nil:
			logf("retention: %v", err)
		case deleted > 0:
			logf("retention: deleted %d requests older than %s", deleted, window)
		}
	}

	prune()

	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
}

type forwarder struct {
	queue  chan *model.Request
	done   chan struct{}
	store  *store.Store
	client *http.Client
	target string
	colors render.Palette
	logf   func(format string, args ...any)
}

func newForwarder(s *store.Store, target string, timeout time.Duration, colors render.Palette, logf func(string, ...any)) *forwarder {
	return &forwarder{
		queue:  make(chan *model.Request, queueSize),
		done:   make(chan struct{}),
		store:  s,
		client: forward.NewClient(timeout),
		target: target,
		colors: colors,
		logf:   logf,
	}
}

func (f *forwarder) orNil() catcher.Forwarder {
	if f == nil {
		return nil
	}

	return f
}

// Forward never blocks the catcher: a full queue drops the forward and says
// which id to replay by hand.
func (f *forwarder) Forward(r *model.Request) {
	select {
	case f.queue <- r:
	default:
		f.logf("forward queue full, #%d saved but not forwarded (use hooktape replay %d)", r.ID, r.ID)
	}
}

func (f *forwarder) start() {
	go func() {
		defer close(f.done)
		for r := range f.queue {
			f.send(r)
		}
	}()
}

func (f *forwarder) stop(wait time.Duration) {
	if f == nil {
		return
	}

	close(f.queue)

	select {
	case <-f.done:
	case <-time.After(wait):
		f.logf("gave up draining the forward queue, %d requests saved but not forwarded", len(f.queue))
	}
}

// send uses its own context because the queue is drained after a signal has
// already cancelled the command's context. The client timeout bounds it.

func (f *forwarder) send(r *model.Request) {
	res := forward.Send(context.Background(), f.client, r, f.target, forward.SendOptions{})

	r.ForwardStatus = res.Status
	r.ForwardMs = res.Duration.Milliseconds()
	if res.Err != nil {
		r.ForwardError = res.Err.Error()
	}

	if err := f.store.SetForwardResult(context.Background(), r.ID, r.ForwardStatus, r.ForwardMs, r.ForwardError); err != nil {
		f.logf("could not record the forward of #%d: %v", r.ID, err)
	}

	f.logf("→ #%d %s %s  %s %dms", r.ID, r.Method, r.Path, appLabel(f.colors, r), r.ForwardMs)
	if verbose {
		f.logf("%s", render.PrettyBody(r.Body, ""))
	}
}
