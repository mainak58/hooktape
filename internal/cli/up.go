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
	"sync"
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
	"github.com/mainak58/hooktape/internal/web"
	"github.com/spf13/cobra"
)

// minHeaderBytes keeps a small --max-body from refusing ordinary webhook headers,
// which would lose the request instead of truncating it.
const minHeaderBytes = 64 << 10

// queueSize is the forward backlog. One worker drains it by default, so forwards
// reach the app in arrival order.
const queueSize = 256

var (
	upForward        string
	upForwardPath    bool
	upForwardWorkers int
	upPort           int
	upAddr           string
	upMaxBody        string
	upRespond        int
	upTimeout        time.Duration
	upNoTunnel       bool
	upTunnelName     string
	upHint           string
	upUIPort         int
	upUIAddr         string
	upNoUI           bool
	upReplayAnyHost  bool
)

var upCmd = &cobra.Command{
	Use:     "up",
	Short:   "Catch webhooks and forward them to your app",
	Example: "  hooktape up --forward http://localhost:3000\n  hooktape up --forward http://localhost:3000 --hint stripe",
	Args:    cobra.NoArgs,
	RunE:    runUp,
}

func init() {
	flags := upCmd.Flags()
	flags.StringVar(&upForward, "forward", "", "URL to forward to (empty means capture only)")
	flags.BoolVar(&upForwardPath, "forward-path", false, "append the path after /h/<endpoint> to --forward, for any caller")
	flags.IntVar(&upForwardWorkers, "forward-workers", 1, "forwards sent at once; above 1 gives up arrival order")
	flags.IntVar(&upPort, "port", 8080, "port to listen on")
	flags.StringVar(&upAddr, "addr", "127.0.0.1", "address for the catcher")
	flags.StringVar(&upMaxBody, "max-body", "1MB", "largest body to store")
	flags.IntVar(&upRespond, "respond", 200, "status to return to the provider")
	flags.DurationVar(&upTimeout, "timeout", 10*time.Second, "timeout for each forward")
	flags.BoolVar(&upNoTunnel, "no-tunnel", false, "do not start a public tunnel")
	flags.StringVar(&upTunnelName, "tunnel-name", "", "name for the public tunnel")
	flags.StringVar(&upHint, "hint", "demo", "endpoint name to show in the paste hint")
	flags.IntVar(&upUIPort, "ui-port", 8081, "port for the dashboard")
	flags.StringVar(&upUIAddr, "ui-addr", "127.0.0.1", "address for the dashboard, which has no login")
	flags.BoolVar(&upNoUI, "no-ui", false, "do not start the dashboard")
	flags.BoolVar(&upReplayAnyHost, "replay-any-host", false, "let the dashboard replay to public hosts, not just local ones")
	rootCmd.AddCommand(upCmd)
}

func runUp(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	started := time.Now()

	maxBody, err := humanize.ParseBytes(upMaxBody)
	if err != nil {
		return fmt.Errorf("invalid --max-body %q", upMaxBody)
	}
	if maxBody == 0 || maxBody > 1<<62 {
		return fmt.Errorf("invalid --max-body %q: use a size above 0", upMaxBody)
	}

	// 1xx is an interim response, so the provider would still get a final 200.
	if upRespond < 200 || upRespond > 599 {
		return fmt.Errorf("invalid --respond %d: use a status from 200 to 599", upRespond)
	}
	if upForwardWorkers < 1 {
		return fmt.Errorf("invalid --forward-workers %d: use 1 or more", upForwardWorkers)
	}
	if strings.HasPrefix(upTunnelName, "-") {
		return fmt.Errorf("invalid --tunnel-name %q", upTunnelName)
	}
	if !upNoUI && upPort != 0 && upUIPort == upPort {
		return fmt.Errorf("--port and --ui-port are both %d; pick two ports", upPort)
	}
	if upForward != "" {
		if upForward, err = forward.CheckTarget(upForward); err != nil {
			return fmt.Errorf("--forward: %w", err)
		}
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(upAddr, strconv.Itoa(upPort)))
	if err != nil {
		return listenError(err, upPort)
	}

	var uiListener net.Listener
	if !upNoUI {
		uiListener, err = net.Listen("tcp", net.JoinHostPort(upUIAddr, strconv.Itoa(upUIPort)))
		if err != nil {
			listener.Close()
			return uiListenError(err, upUIPort)
		}
		defer uiListener.Close()
	}

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
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    int(min(max(maxBody, minHeaderBytes), http.DefaultMaxHeaderBytes)),
	}

	var ui *web.Handler
	var uiSrv *http.Server
	if uiListener != nil {
		newClient := forward.NewLocalClient
		if upReplayAnyHost {
			newClient = forward.NewClient
		}
		ui = &web.Handler{Store: s, Client: newClient(upTimeout), KeepPath: upForwardPath, Logf: logf}
		ui.SetStatus(uiStatus(started, localURL(listener.Addr()), ""))
		uiSrv = &http.Server{
			Handler:           ui.Routes(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      max(30*time.Second, upTimeout+5*time.Second), // a replay answers inside the handler
			IdleTimeout:       60 * time.Second,
		}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	printBanner(cmd, colors, listener.Addr(), uiListener)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	if uiSrv != nil {
		go func() {
			if err := uiSrv.Serve(uiListener); !errors.Is(err, http.ErrServerClosed) {
				logf("%s", colors.Red(fmt.Sprintf("dashboard stopped: %v", err)))
				logf("%s", colors.Dim("the catcher is still running"))
			}
		}()
	}

	// The tunnel comes up after the server, so cloudflared's first probe of the
	// local URL already has something to reach.
	var tun *tunnel.Tunnel
	var tunnelErr <-chan error
	if !upNoTunnel {
		tun, tunnelErr = startTunnel(ctx, cmd, colors, logf, localURL(listener.Addr()))
		if tun != nil {
			ui.SetStatus(uiStatus(started, localURL(listener.Addr()), tun.PublicURL))
		}
	}

	// A dead catcher still takes the full shutdown path, so cloudflared is not
	// left holding a public URL and queued forwards are accounted for.
	var exitErr error
	select {
	case exitErr = <-serveErr:
	case err := <-tunnelErr:
		logf("%s", colors.Red(fmt.Sprintf("cloudflared exited: %v", err)))
		logf("%s", colors.Dim("the catcher is still running, but the public URL is gone"))
		tun = nil
		select {
		case exitErr = <-serveErr:
		case <-ctx.Done():
		}
		logf("")
		logf("shutting down")
	case <-ctx.Done():
		logf("")
		logf("shutting down")
	}

	// From here a second Ctrl+C kills the process, since the drain below can take
	// --timeout per step.
	stop()

	// cloudflared goes first so it stops sending requests at a server that is
	// about to close.
	if err := tun.Stop(); err != nil {
		logf("stopping cloudflared: %v", err)
	}

	// Shutdown abandons a stuck handler at its deadline, so a Forward can still
	// arrive after stop. forwarder.closed exists for that call. A forward or a
	// dashboard replay can take --timeout, and each step gets all of it.
	grace := max(5*time.Second, upTimeout)
	shutdown := func(name string, server *http.Server) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logf("%s: %v", name, err)
		}
	}
	shutdown("shutdown", srv)
	if uiSrv != nil {
		shutdown("dashboard shutdown", uiSrv)
	}
	fwd.stop(grace)

	if errors.Is(exitErr, http.ErrServerClosed) {
		return nil
	}

	return exitErr
}

func printBanner(cmd *cobra.Command, colors render.Palette, addr net.Addr, uiListener net.Listener) {
	out := cmd.OutOrStdout()

	target := upForward
	if target == "" {
		target = colors.Dim("capture only (no --forward)")
	}

	fmt.Fprintln(out, colors.Bold("hooktape up"))
	fmt.Fprintf(out, "  db        %s\n", dbPath)
	fmt.Fprintf(out, "  catcher   %s/h/<endpoint>/...\n", shownURL(upAddr, addr))
	fmt.Fprintf(out, "  forward   %s\n", target)

	if uiListener != nil {
		fmt.Fprintf(out, "  dashboard %s\n", shownURL(upUIAddr, uiListener.Addr()))
	} else {
		fmt.Fprintf(out, "  dashboard %s\n", colors.Dim("off (--no-ui)"))
	}

	if !isLoopback(upAddr) {
		fmt.Fprintln(out, colors.Dim("  warning   anyone on this network can reach the catcher"))
	}
	if uiListener != nil && !isLoopback(upUIAddr) {
		fmt.Fprintln(out, colors.Dim("  warning   anyone on this network can read, replay and delete from the dashboard"))
	}

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
// any endpoint name, so --hint only picks which one is suggested.
func pasteURL(base string) string {
	return base + "/h/" + upHint
}

// listenError spells out the fix for the one listen failure that has an obvious
// one, because the raw bind error buries it.
func listenError(err error, port int) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("port %d is busy; try --port %d", port, port+1)
	}

	return err
}

// shownURL is the address as configured, because the wildcard a listener reports
// cannot be pasted anywhere.
func shownURL(configured string, addr net.Addr) string {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String()
	}

	return "http://" + net.JoinHostPort(configured, port)
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)

	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// uiStatus is what the dashboard shows about this run. Only the public URL
// changes after startup, once cloudflared reports one.
func uiStatus(started time.Time, catcher, public string) web.Status {
	v, _ := buildVersion()

	return web.Status{
		Version:      v,
		DB:           dbPath,
		Catcher:      catcher,
		Public:       public,
		Forward:      upForward,
		EndpointHint: upHint,
		StartedAt:    started.UnixMilli(),
	}
}

func uiListenError(err error, port int) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("dashboard port %d is busy; try --ui-port %d or --no-ui", port, port+1)
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

// forwarder never closes queue: a handler Shutdown gave up on may still send to
// it, and a send on a closed channel panics. closed is the stop signal instead.
type forwarder struct {
	queue     chan *model.Request
	closed    chan struct{}
	closeOnce sync.Once
	done      chan struct{}
	savedOnce sync.Once
	store     *store.Store
	client    *http.Client
	target    string
	colors    render.Palette
	logf      func(format string, args ...any)

	mu      sync.Mutex
	sending map[int64]bool
}

func newForwarder(s *store.Store, target string, timeout time.Duration, colors render.Palette, logf func(string, ...any)) *forwarder {
	return &forwarder{
		queue:   make(chan *model.Request, queueSize),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
		store:   s,
		client:  forward.NewClient(timeout),
		target:  target,
		colors:  colors,
		logf:    logf,
		sending: map[int64]bool{},
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
	case <-f.closed:
		f.logf("shutting down, #%d saved but not forwarded (use hooktape replay %d)", r.ID, r.ID)
		return
	default:
	}

	select {
	case f.queue <- r:
	default:
		f.logf("forward queue full, #%d saved but not forwarded (use hooktape replay %d)", r.ID, r.ID)
	}
}

func (f *forwarder) start() {
	var workers sync.WaitGroup
	for range upForwardWorkers {
		workers.Go(f.work)
	}

	go func() {
		workers.Wait()
		close(f.done)
	}()
}

// work runs until stop, then finishes what is queued.
func (f *forwarder) work() {
	for {
		select {
		case r := <-f.queue:
			f.send(r)
		case <-f.closed:
			select {
			case r := <-f.queue:
				f.send(r)
			default:
				return
			}
		}
	}
}

func (f *forwarder) stop(wait time.Duration) {
	if f == nil {
		return
	}

	f.closeOnce.Do(func() { close(f.closed) })

	select {
	case <-f.done:
	case <-time.After(wait):
	}

	if ids := f.unsent(); len(ids) > 0 {
		f.logf("gave up draining the forward queue, saved but not forwarded: %s (use hooktape replay)", strings.Join(ids, " "))
	}
}

// unsent empties the queue and adds whatever a worker is still stuck sending, so
// the log names every row that did not make it.
func (f *forwarder) unsent() []string {
	var ids []string

	f.mu.Lock()
	for id := range f.sending {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	f.mu.Unlock()

	for {
		select {
		case r := <-f.queue:
			ids = append(ids, strconv.FormatInt(r.ID, 10))
		default:
			return ids
		}
	}
}

// send uses its own context because the queue is drained after a signal has
// already cancelled the command's context. The client timeout bounds it.

func (f *forwarder) send(r *model.Request) {
	f.mu.Lock()
	f.sending[r.ID] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.sending, r.ID)
		f.mu.Unlock()
	}()

	res := forward.Send(context.Background(), f.client, r, f.target, forward.SendOptions{KeepPath: upForwardPath})
	if res.Status != 0 {
		f.savedOnce.Do(func() { _ = config.Save(config.Config{LastForward: f.target}) })
	}

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
		f.logf("%s", render.PrettyBody(r.Body))
	}
}
