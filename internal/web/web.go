// Package web serves the local dashboard: one embedded HTML page plus the
// small JSON API it reads. It runs on its own port so the catcher port stays
// exactly what a provider sees.
package web

import (
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	_ "embed"

	"github.com/mainak58/hooktape/internal/forward"
	"github.com/mainak58/hooktape/internal/render"
	"github.com/mainak58/hooktape/internal/store"
)

//go:embed index.html
var page []byte

// maxList caps a list request so a huge database cannot be pulled into memory
// in one go by a stray ?limit=.
const maxList = 10000

// Status is what the page shows in its header and Server tab.
type Status struct {
	Version      string `json:"version"`
	DB           string `json:"db"`
	Catcher      string `json:"catcher"`
	Public       string `json:"public"` // "" = local only
	Forward      string `json:"forward"`
	EndpointHint string `json:"endpoint_hint"`
	StartedAt    int64  `json:"started_at"` // unix milliseconds
}

type Handler struct {
	Store    *store.Store
	Client   *http.Client // used for replays from the page
	KeepPath bool         // send the captured path too, as --forward-path does
	Logf     func(format string, args ...any)

	status atomic.Pointer[Status]
}

// SetStatus is called again once the tunnel URL is known, so it tolerates a
// nil handler the same way the rest of the up command does.
func (h *Handler) SetStatus(s Status) {
	if h == nil {
		return
	}

	h.status.Store(&s)
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.index)
	mux.HandleFunc("GET /api/status", h.getStatus)
	mux.HandleFunc("GET /api/requests", h.listRequests)
	mux.HandleFunc("GET /api/requests/{id}", h.getRequest)
	mux.HandleFunc("POST /api/requests/{id}/replay", h.replayRequest)
	mux.HandleFunc("POST /api/requests/delete", h.deleteRequests)

	// The API has no auth, so what keeps other sites out is that a browser
	// labels their requests: CrossOriginProtection refuses cross-site writes,
	// and localHostOnly refuses a rebound hostname, which a browser counts as
	// same-origin and would let read every stored secret.
	return localHostOnly(http.NewCrossOriginProtection().Handler(mux))
}

func localHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")

		if !strings.EqualFold(host, "localhost") && net.ParseIP(host) == nil {
			http.Error(w, "the dashboard answers to localhost or an IP address only", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(page)
}

func (h *Handler) getStatus(w http.ResponseWriter, r *http.Request) {
	s := h.status.Load()
	if s == nil {
		s = &Status{}
	}

	writeJSON(w, s)
}

func (h *Handler) listRequests(w http.ResponseWriter, r *http.Request) {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = 500
	}
	if limit > maxList {
		limit = maxList
	}

	requests, err := h.Store.List(r.Context(), store.ListOptions{
		Endpoint: r.URL.Query().Get("endpoint"),
		Limit:    limit,
	})
	if err != nil {
		h.fail(w, err)
		return
	}

	writeJSON(w, requests)
}

func (h *Handler) getRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := requestID(w, r)
	if !ok {
		return
	}

	req, err := h.Store.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}

	writeJSON(w, req)
}

func (h *Handler) replayRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := requestID(w, r)
	if !ok {
		return
	}

	var body struct {
		Target string `json:"target"`
	}
	if !readJSON(w, r, &body) {
		return
	}

	target, err := forward.CheckTarget(body.Target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req, err := h.Store.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}

	res := forward.Send(r.Context(), h.Client, req, target, forward.SendOptions{Replay: true, KeepPath: h.KeepPath})

	errMsg := ""
	if res.Err != nil {
		errMsg = res.Err.Error()
	}
	recordErr := ""
	if err := h.Store.SetReplayResult(r.Context(), id, res.Status, res.Duration.Milliseconds(), errMsg); err != nil {
		h.logf("could not record the replay of #%d: %v", id, err)
		recordErr = "replay sent, but its result was not saved"
	}

	h.logf("↻ #%d replayed → %s %dms", id, render.ForwardLabel(res.Status, errMsg), res.Duration.Milliseconds())

	writeJSON(w, map[string]any{
		"status":       res.Status,
		"ms":           res.Duration.Milliseconds(),
		"error":        errMsg,
		"body":         string(res.Body),
		"record_error": recordErr,
	})
}

func (h *Handler) deleteRequests(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs         []int64 `json:"ids"`
		OlderThanMs int64   `json:"older_than_ms"`
		All         bool    `json:"all"`
	}
	if !readJSON(w, r, &body) {
		return
	}

	selectors := 0
	for _, set := range []bool{body.All, body.OlderThanMs != 0, len(body.IDs) > 0} {
		if set {
			selectors++
		}
	}
	if selectors > 1 {
		http.Error(w, "pass only one of ids, older_than_ms, or all", http.StatusBadRequest)
		return
	}

	var deleted int64
	var err error
	switch {
	case body.All:
		deleted, err = h.Store.DeleteMatching(r.Context(), store.DeleteFilter{})
	case body.OlderThanMs > 0:
		deleted, err = h.Store.DeleteMatching(r.Context(), store.DeleteFilter{ToMs: body.OlderThanMs})
	case len(body.IDs) > 0:
		deleted, err = h.Store.DeleteByIDs(r.Context(), body.IDs)
	default:
		http.Error(w, "nothing to delete: pass ids, older_than_ms, or all", http.StatusBadRequest)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}

	h.logf("deleted %d request(s) from the dashboard", deleted)

	writeJSON(w, map[string]int64{"deleted": deleted})
}

func (h *Handler) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

// fail keeps store errors off the page, since they can name the database path
// and the page shows whatever comes back in a toast. The detail goes to the log.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	h.logf("dashboard: %v", err)
	http.Error(w, "storage error, see the hooktape log", http.StatusInternalServerError)
}

func requestID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return 0, false
	}

	return id, true
}

func readJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dest); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}

	return true
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
