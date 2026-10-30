package catcher

import (
	"net/http"

	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/store"
)

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

func (h *Handler) Routes() http.Handler
