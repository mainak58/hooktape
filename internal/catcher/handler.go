package catcher

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/store"
)

var validEndpoint = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type Forwarder interface {
	Forward(r *model.Request) // called async after save; must not block the handler
}

type Handler struct {
	Store     *store.Store
	Forwarder Forwarder // may be nil (capture-only)
	MaxBody   int64     // bytes; default 1<<20
	Respond   int       // status to return to provider; default 200
	Logf      func(format string, args ...any)
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/h/{endpoint}", h.capture)
	mux.HandleFunc("/h/{endpoint}/{rest...}", h.capture)
	mux.HandleFunc("GET /_health", h.health)
	mux.HandleFunc("/", h.notFound)
	return mux
}

func (h *Handler) capture(w http.ResponseWriter, r *http.Request) {
	maxBody := h.MaxBody
	if maxBody <= 0 {
		maxBody = 1 << 20
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}

	headers := r.Header.Clone()
	headers.Set("Host", r.Host)

	endpoint := r.PathValue("endpoint")
	if !validEndpoint.MatchString(endpoint) {
		http.Error(w, "invalid endpoint name", http.StatusBadRequest)
		return
	}

	_, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/h/"), "/")

	req := &model.Request{
		Endpoint:  endpoint,
		Method:    r.Method,
		Path:      "/" + rest,
		Query:     r.URL.RawQuery,
		Headers:   headers,
		Body:      body,
		RemoteIP:  clientIP(r),
		CreatedAt: time.Now().UnixMilli(),
	}

	id, err := h.Store.Insert(r.Context(), req)
	if err != nil {
		h.logf("insert failed: %v", err)
		http.Error(w, "could not store request", http.StatusInternalServerError)
		return
	}

	status := h.Respond
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"ok":true,"id":%d}`, id)

	if h.Forwarder != nil {
		forwarded := *req
		h.Forwarder.Forward(&forwarded)
	}

	h.logf("← #%d %s %s  %d B", id, req.Method, req.Path, len(body))
}

func (h *Handler) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "hooktape: send webhooks to /h/<endpoint>/...", http.StatusNotFound)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if forwarded := r.Header.Get("CF-Connecting-IP"); forwarded != "" {
			return forwarded
		}
	}

	return host
}
