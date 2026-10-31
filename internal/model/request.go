package model

type Request struct {
	ID            int64               `json:"id"`
	Endpoint      string              `json:"endpoint"`
	Method        string              `json:"method"`
	Path          string              `json:"path"`  // path AFTER /h/{endpoint}, always starts with "/"
	Query         string              `json:"query"` // raw query without "?"
	Headers       map[string][]string `json:"headers"`
	Body          []byte              `json:"body,omitempty"`
	BodySize      int                 `json:"body_size"`
	RemoteIP      string              `json:"remote_ip"`
	CreatedAt     int64               `json:"created_at"`     // unix milliseconds
	ForwardStatus int                 `json:"forward_status"` // 0 = not attempted
	ForwardError  string              `json:"forward_error"`  // "" if none
	ForwardMs     int64               `json:"forward_ms"`
	ReplayStatus  int                 `json:"replay_status"` // latest replay; 0 = none
	ReplayError   string              `json:"replay_error"`  // "" if none
	ReplayMs      int64               `json:"replay_ms"`
	Truncated     bool                `json:"truncated"` // body was over --max-body when caught
}
