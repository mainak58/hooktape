package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"os/signal"
	"runtime"
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
	"github.com/mainak58/hooktape/internal/tunnel"
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
	upEndpoint   string
)

var upCmd = &cobra.Command{
	Use:     "up",
	Short:   "Catch webhooks and forward them to your app",
	Example: "  hooktape up --forward http://localhost:3000\n  hooktape up --forward http://localhost:3000 --endpoint stripe",
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
	flags.StringVar(&upEndpoint, "endpoint", "demo", "endpoint name to show in the paste hint")
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
		// The suggested port is the whole message, so a usage dump above it only
		// gets in the way.
		cmd.SilenceUsage = true
		return listenError(err, upPort)
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

	printBanner(cmd, colors, listener.Addr())

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()

	// The tunnel comes up after the server, so cloudflared's first probe of the
	// local URL already has something to reach.
	var tun *tunnel.Tunnel
	var tunnelErr <-chan error
	if !upNoTunnel {
		tun, tunnelErr = startTunnel(ctx, cmd, colors, logf, localURL(listener.Addr()))
	}

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case err := <-tunnelErr:
		logf("%s", colors.Red(fmt.Sprintf("cloudflared exited: %v", err)))
		logf("%s", colors.Dim("the catcher is still running, but the public URL is gone"))
		tun = nil
		<-ctx.Done()
		logf("")
		logf("shutting down")
	case <-ctx.Done():
		logf("")
		logf("shutting down")
	}

	// cloudflared goes first so it stops sending requests at a server that is
	// about to close.
	if err := tun.Stop(); err != nil {
		logf("stopping cloudflared: %v", err)
	}

	// Shutdown waits for handlers to return, so no further Forward call can
	// arrive once the queue is closed.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logf("shutdown: %v", err)
	}
	fwd.stop(5 * time.Second)

	return nil
}

func printBanner(cmd *cobra.Command, colors render.Palette, addr net.Addr) {
	out := cmd.OutOrStdout()

	target := upForward
	if target == "" {
		target = colors.Dim("capture only (no --forward)")
	}

	fmt.Fprintln(out, colors.Bold("hooktape up"))
	fmt.Fprintf(out, "  db        %s\n", dbPath)
	fmt.Fprintf(out, "  catcher   http://%s/h/<endpoint>/...\n", addr.String())
	fmt.Fprintf(out, "  forward   %s\n", target)

	if upNoTunnel {
		fmt.Fprintf(out, "  public    %s\n", colors.Dim("off (--no-tunnel)"))
		fmt.Fprintf(out, "  paste →   %s\n", pasteURL(localURL(addr)))
		fmt.Fprintln(out)
		return
	}

	fmt.Fprintln(out, colors.Dim("  starting tunnel…"))
}

// startTunnel brings cloudflared up and prints where to paste the public URL.
// A missing binary or a failed start is not fatal: the catcher still works for
// anything that can reach this machine.
func startTunnel(ctx context.Context, cmd *cobra.Command, colors render.Palette, logf func(string, ...any), local string) (*tunnel.Tunnel, <-chan error) {
	out := cmd.OutOrStdout()

	var lines func(string, ...any)
	if verbose {
		lines = logf
	}

	tun, errs, err := tunnel.Start(ctx, tunnel.Options{
		LocalURL:   local,
		TunnelName: upTunnelName,
		Logf:       lines,
	})
	if errors.Is(err, exec.ErrNotFound) {
		fmt.Fprintf(out, "  public    %s\n", colors.Dim("local only (cloudflared not found)"))
		fmt.Fprintf(out, "  paste →   %s\n", pasteURL(local))
		printInstallHint(out, colors)
		return nil, nil
	}
	// A cancelled context means Ctrl+C arrived mid-startup, and the shutdown
	// path says so already.
	if errors.Is(err, context.Canceled) {
		return nil, nil
	}
	if err != nil {
		fmt.Fprintf(out, "  public    %s\n", colors.Red("tunnel failed: "+err.Error()))
		fmt.Fprintf(out, "  paste →   %s\n", pasteURL(local))
		fmt.Fprintln(out)
		return nil, nil
	}

	if tun.PublicURL != "" {
		fmt.Fprintf(out, "  public    %s\n", tun.PublicURL)
		fmt.Fprintf(out, "  paste →   %s\n", pasteURL(tun.PublicURL))
		fmt.Fprintln(out, colors.Dim("  quick tunnel URL changes on every restart and can take a few seconds to resolve; use --tunnel-name for a stable URL"))
	} else {
		fmt.Fprintf(out, "  public    %s\n", colors.Dim("tunnel "+upTunnelName+" connected (hostname is set in Cloudflare)"))
	}
	fmt.Fprintln(out)

	return tun, errs
}

func printInstallHint(out io.Writer, colors render.Palette) {
	install := "see the download page below"
	switch runtime.GOOS {
	case "darwin":
		install = "brew install cloudflared"
	case "windows":
		install = "winget install --id Cloudflare.cloudflared"
	case "linux":
		install = "see the download page below, or your package manager"
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, colors.Dim("  to get a public URL, install cloudflared:"))
	fmt.Fprintf(out, "    %s\n", install)
	fmt.Fprintln(out, colors.Dim("    https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"))
	fmt.Fprintln(out)
}

// pasteURL is the hint to paste into a provider's dashboard. Routing accepts
// any endpoint name, so --endpoint only picks which one is suggested.
func pasteURL(base string) string {
	return base + "/h/" + upEndpoint
}

// listenError spells out the fix for the one listen failure that has an obvious
// one, because the raw bind error buries it.
func listenError(err error, port int) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("port %d is busy; try --port %d", port, port+1)
	}

	return err
}

// localURL is the address cloudflared should forward to. A listener on every
// interface still has to be reached by a real one.
func localURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String()
	}

	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}

	return "http://" + net.JoinHostPort(host, port)
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
