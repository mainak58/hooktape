package forward

import (
	"context"
	"net/http"
	"time"

	"github.com/mainak58/hooktape/internal/model"
)

type Result struct {
	Status   int // 0 if request failed before a response
	Duration time.Duration
	Body     []byte 
	Err      error 
}

func Send(ctx context.Context, client *http.Client, r *model.Request, target string) Result
func NewClient(timeout time.Duration) *http.Client // no redirects followed
