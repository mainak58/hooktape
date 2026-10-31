package render

import (
	"strings"
	"testing"
)

func TestColors(t *testing.T) {
	if got := Colors(false).Green("200"); got != "200" {
		t.Errorf("disabled: got %q", got)
	}
	if got := Colors(true).Green("200"); got != "\033[32m200\033[0m" {
		t.Errorf("enabled: got %q", got)
	}

	on, off := Colors(true), Colors(false)
	for name, styles := range map[string][2]func(string) string{
		"Dim":    {on.Dim, off.Dim},
		"Green":  {on.Green, off.Green},
		"Red":    {on.Red, off.Red},
		"Yellow": {on.Yellow, off.Yellow},
		"Cyan":   {on.Cyan, off.Cyan},
		"Bold":   {on.Bold, off.Bold},
	} {
		if styles[0]("x") == "x" {
			t.Errorf("%s: enabled returned plain text", name)
		}
		if styles[1]("x") != "x" {
			t.Errorf("%s: disabled changed the text", name)
		}
	}
}

func TestPrettyBody(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"empty", nil, "<empty body>"},
		{"json", []byte(`{"a":1}`), "{\n  \"a\": 1\n}"},
		{"text", []byte("hello=world"), "hello=world"},
		{"tabs and newlines stay text", []byte("a\tb\r\nc"), "a\tb\r\nc"},
		{"control bytes go to hex", []byte("A\x00\x00B"), "<4 bytes binary, first 32 bytes hex: 41000042>"},
		{"escape sequences go to hex", []byte("\x1b[2J"), "<4 bytes binary, first 32 bytes hex: 1b5b324a>"},
		{"binary", []byte{0xff, 0x00, 0xfe, 'a'}, "<4 bytes binary, first 32 bytes hex: ff00fe61>"},
		{
			"long binary shows only 32 bytes",
			append([]byte{0xff}, make([]byte, 39)...),
			"<40 bytes binary, first 32 bytes hex: ff" + strings.Repeat("00", 31) + ">",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PrettyBody(tt.body); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestForwardLabel(t *testing.T) {
	tests := []struct {
		status int
		errMsg string
		want   string
	}{
		{0, "", "-"},
		{200, "", "200"},
		{500, "", "500"},
		{0, "dial tcp 127.0.0.1:3000: connect: connection refused", "refused"},
		{0, `Post "http://localhost:3000": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`, "timeout"},
		{0, "dial tcp: i/o timeout", "timeout"},
		{0, "something else went wrong", "error"},
		{200, "unexpected EOF", "error"},
	}

	for _, tt := range tests {
		if got := ForwardLabel(tt.status, tt.errMsg); got != tt.want {
			t.Errorf("ForwardLabel(%d, %q) = %q, want %q", tt.status, tt.errMsg, got, tt.want)
		}
	}
}
