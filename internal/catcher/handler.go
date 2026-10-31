package catcher

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
	MaxBody   int64     // bytes; default 1MB (SI, as --max-body parses it)
	Respond   int       // status to return to provider; default 200
	Logf      func(format string, args ...any)
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_health", h.health)
	mux.HandleFunc("/", h.notFound)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/h/"); ok && rest != "" {
			h.capture(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (h *Handler) capture(w http.ResponseWriter, r *http.Request) {
	maxBody := h.MaxBody
	if maxBody <= 0 {
		maxBody = 1_000_000
	}

	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if readErr != nil && len(body) == 0 {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}


	truncated := readErr != nil || int64(len(body)) > maxBody
	if int64(len(body)) > maxBody {
		body = body[:maxBody]
	}

	headers := r.Header.Clone()
	headers.Set("Host", r.Host)

	endpoint, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/h/"), "/")
	endpoint, _ = url.PathUnescape(endpoint)
	if !validEndpoint.MatchString(endpoint) {
		http.Error(w, "invalid endpoint name", http.StatusBadRequest)
		return
	}

	req := &model.Request{
		Endpoint:  endpoint,
		Method:    r.Method,
		Path:      "/" + rest,
		Query:     r.URL.RawQuery,
		Headers:   headers,
		Body:      body,
		RemoteIP:  clientIP(r),
		CreatedAt: time.Now().UnixMilli(),
		Truncated: truncated,
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 25*time.Second)
	defer cancel()

	id, err := h.Store.Insert(ctx, req)
	if err != nil {
		h.logf("insert failed: %v", err)
		http.Error(w, "could not store request", http.StatusInternalServerError)
		return
	}

	status := h.Respond
	if status == 0 {
		status = http.StatusOK
	}
	reply := fmt.Sprintf(`{"ok":true,"id":%d}`, id)

	// 413 even with --respond: a 200 would tell the provider nothing was lost,
	// and this way it can retry once --max-body is raised.
	if truncated {
		status = http.StatusRequestEntityTooLarge
		reply = fmt.Sprintf(`{"ok":false,"id":%d,"truncated":true}`, id)
		h.logf("#%d is over --max-body, stored the first %d B and answered 413", id, maxBody)
	}
	if readErr != nil {
		status = http.StatusBadRequest
		h.logf("#%d was cut off (%v), stored the %d B that arrived", id, readErr, len(body))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, reply)

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
		if forwarded := r.Header.Get("CF-Connecting-IP"); net.ParseIP(forwarded) != nil {
			return forwarded
		}
	}

	return host
}
