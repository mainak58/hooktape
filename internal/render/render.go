package render

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Palette struct {
	Dim, Green, Red, Yellow, Cyan, Bold func(string) string
}

func Colors(enabled bool) Palette {
	style := func(code string) func(string) string {
		if !enabled {
			return func(s string) string { return s }
		}
		return func(s string) string { return "\033[" + code + "m" + s + "\033[0m" }
	}

	return Palette{
		Dim:    style("2"),
		Green:  style("32"),
		Red:    style("31"),
		Yellow: style("33"),
		Cyan:   style("36"),
		Bold:   style("1"),
	}
}

func PrettyBody(body []byte) string {
	if len(body) == 0 {
		return "<empty body>"
	}

	if json.Valid(body) {
		var out bytes.Buffer
		if err := json.Indent(&out, body, "", "  "); err == nil {
			return out.String()
		}
	}

	if utf8.Valid(body) && !bytes.ContainsFunc(body, isControl) {
		return string(body)
	}

	return fmt.Sprintf("<%d bytes binary, first 32 bytes hex: %s>", len(body), hex.EncodeToString(body[:min(len(body), 32)]))
}

func isControl(r rune) bool {
	return r < 0x20 && r != '\t' && r != '\r' && r != '\n'
}

func ForwardLabel(status int, errMsg string) string {
	if errMsg == "" {
		if status == 0 {
			return "-"
		}
		return strconv.Itoa(status)
	}

	msg := strings.ToLower(errMsg)
	switch {
	case strings.Contains(msg, "refused"):
		return "refused"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	default:
		return "error"
	}
}
