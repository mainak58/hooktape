// Package web serves the local dashboard: one embedded HTML page plus the
// small JSON API it reads. It runs on its own port so the catcher port stays
// exactly what a provider sees.
package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"

	_ "embed"

	"github.com/mainak58/hooktape/internal/forward"
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
	Retention    string `json:"retention"`  // "" = keep forever
}

type Handler struct {
	Store  *store.Store
	Client *http.Client // used for replays from the page
	Logf   func(format string, args ...any)

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
	return mux
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

	target, err := checkTarget(body.Target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req, err := h.Store.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}

	res := forward.Send(r.Context(), h.Client, req, target, forward.SendOptions{Replay: true})

	errMsg := ""
	if res.Err != nil {
		errMsg = res.Err.Error()
	}
	if err := h.Store.SetForwardResult(r.Context(), id, res.Status, res.Duration.Milliseconds(), errMsg); err != nil {
		h.logf("could not record the replay of #%d: %v", id, err)
	}

	h.logf("↻ #%d replayed → %s %dms", id, replayLabel(res.Status, errMsg), res.Duration.Milliseconds())

	writeJSON(w, map[string]any{
		"status": res.Status,
		"ms":     res.Duration.Milliseconds(),
		"error":  errMsg,
		"body":   string(res.Body),
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

	var deleted int64
	var err error
	switch {
	case body.All:
		deleted, err = h.Store.DeleteAll(r.Context())
	case body.OlderThanMs > 0:
		deleted, err = h.Store.DeleteOlderThan(r.Context(), body.OlderThanMs)
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

// fail keeps store errors off the page except as a short message, since the
// page shows whatever comes back in a toast.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "request not found", http.StatusNotFound)
		return
	}

	h.logf("dashboard: %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func requestID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return 0, false
	}

	return id, true
}

// checkTarget validates the replay target typed into the page: it is a URL
// this process will fetch, so it has to be an http(s) address and nothing else.
func checkTarget(target string) (string, error) {
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

func readJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
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

func replayLabel(status int, errMsg string) string {
	if errMsg != "" {
		return errMsg
	}

	return strconv.Itoa(status)
}
