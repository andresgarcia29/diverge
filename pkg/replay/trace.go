package replay

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RequestEntry represents a single recorded HTTP, ConnectRPC, or gRPC/Protobuf request in a replay trace.
type RequestEntry struct {
	ID           string            `json:"id,omitempty"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	URL          string            `json:"url,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body,omitempty"`
	BodyBase64   string            `json:"body_base64,omitempty"`
	ProtoMessage string            `json:"proto_message,omitempty"`
	ExpectStatus int               `json:"expect_status,omitempty"`
}

// rawRequestEntry supports both snake_case and camelCase JSON keys when unmarshaling trace entries.
type rawRequestEntry struct {
	ID               string            `json:"id,omitempty"`
	Method           string            `json:"method"`
	Path             string            `json:"path"`
	URL              string            `json:"url"`
	Headers          map[string]string `json:"headers,omitempty"`
	Body             string            `json:"body,omitempty"`
	BodyBase64       string            `json:"body_base64,omitempty"`
	BodyBase64Camel  string            `json:"bodyBase64,omitempty"`
	ProtoMessage     string            `json:"proto_message,omitempty"`
	ProtoMsgCamel    string            `json:"protoMessage,omitempty"`
	ExpectStatus     int               `json:"expect_status,omitempty"`
	ExpectStatusCame int               `json:"expectStatus,omitempty"`
}

func (r rawRequestEntry) normalize(index int) (RequestEntry, error) {
	target := strings.TrimSpace(r.Path)
	if target == "" {
		target = strings.TrimSpace(r.URL)
	}
	if target == "" {
		return RequestEntry{}, fmt.Errorf("trace entry %d is missing path or url", index)
	}

	method := strings.ToUpper(strings.TrimSpace(r.Method))
	bodyB64 := strings.TrimSpace(r.BodyBase64)
	if bodyB64 == "" {
		bodyB64 = strings.TrimSpace(r.BodyBase64Camel)
	}
	if method == "" {
		if r.Body != "" || bodyB64 != "" {
			method = "POST"
		} else {
			method = "GET"
		}
	}

	protoMsg := strings.TrimSpace(r.ProtoMessage)
	if protoMsg == "" {
		protoMsg = strings.TrimSpace(r.ProtoMsgCamel)
	}

	expectStatus := r.ExpectStatus
	if expectStatus == 0 {
		expectStatus = r.ExpectStatusCame
	}

	var headers map[string]string
	if len(r.Headers) > 0 {
		headers = make(map[string]string, len(r.Headers))
		for k, v := range r.Headers {
			trimmedKey := strings.TrimSpace(k)
			if trimmedKey != "" {
				headers[trimmedKey] = v
			}
		}
	}

	id := strings.TrimSpace(r.ID)
	if id == "" {
		id = fmt.Sprintf("req-%d", index+1)
	}

	return RequestEntry{
		ID:           id,
		Method:       method,
		Path:         target,
		Headers:      headers,
		Body:         r.Body,
		BodyBase64:   bodyB64,
		ProtoMessage: protoMsg,
		ExpectStatus: expectStatus,
	}, nil
}

// PayloadBytes returns the raw request body bytes, decoding BodyBase64 if set.
func (e RequestEntry) PayloadBytes() ([]byte, error) {
	if e.BodyBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(e.BodyBase64))
		if err != nil {
			return nil, fmt.Errorf("invalid base64 body in entry %q: %w", e.ID, err)
		}
		return decoded, nil
	}
	if e.Body != "" {
		return []byte(e.Body), nil
	}
	return nil, nil
}

// harArchive models the subset of HTTP Archive (HAR 1.2) needed for request replay.
type harArchive struct {
	Log *struct {
		Entries []harEntry `json:"entries"`
	} `json:"log"`
}

type harEntry struct {
	Pageref string     `json:"pageref,omitempty"`
	Request harRequest `json:"request"`
}

type harRequest struct {
	Method   string      `json:"method"`
	URL      string      `json:"url"`
	Headers  []harHeader `json:"headers"`
	PostData *struct {
		MimeType string `json:"mimeType"`
		Text     string `json:"text"`
		Encoding string `json:"encoding,omitempty"`
		Comment  string `json:"comment,omitempty"`
	} `json:"postData,omitempty"`
}

type harHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// LoadTrace loads a replay trace from a file path (or stdin when path is "-").
func LoadTrace(path string) ([]RequestEntry, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errors.New("trace file path is required")
	}
	var (
		data []byte
		err  error
	)
	if trimmed == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(trimmed)
	}
	if err != nil {
		return nil, fmt.Errorf("read trace %q: %w", path, err)
	}
	return ParseTrace(data, filepath.Ext(trimmed))
}

// ParseTrace parses a request trace from raw bytes in HAR 1.2 (.har), JSON array/wrapper (.json), or JSONL (.jsonl) format.
func ParseTrace(data []byte, extHint string) ([]RequestEntry, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("trace is empty")
	}

	lowerExt := strings.ToLower(strings.TrimSpace(extHint))
	if lowerExt == ".har" {
		return parseHAR(trimmed)
	}

	// Auto-detect JSON array vs JSON object (HAR or {"requests": [...]}) vs JSONL
	if trimmed[0] == '[' {
		var rawList []rawRequestEntry
		if err := json.Unmarshal(trimmed, &rawList); err != nil {
			return nil, fmt.Errorf("parse JSON trace array: %w", err)
		}
		return normalizeRawEntries(rawList)
	}

	if trimmed[0] == '{' && !bytes.Contains(trimmed, []byte("\n{")) {
		// Check if it is a HAR archive or a wrapper {"requests": [...]}
		var probe struct {
			Log      *json.RawMessage  `json:"log"`
			Requests []rawRequestEntry `json:"requests"`
		}
		if err := json.Unmarshal(trimmed, &probe); err == nil {
			if probe.Log != nil {
				return parseHAR(trimmed)
			}
			if probe.Requests != nil {
				return normalizeRawEntries(probe.Requests)
			}
		}
	}

	return parseJSONL(trimmed)
}

func normalizeRawEntries(rawList []rawRequestEntry) ([]RequestEntry, error) {
	if len(rawList) == 0 {
		return nil, errors.New("trace contains no request entries")
	}
	out := make([]RequestEntry, 0, len(rawList))
	for i, r := range rawList {
		entry, err := r.normalize(i)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

func parseJSONL(data []byte) ([]RequestEntry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Allow up to 10MB per JSONL line for large payloads
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var entries []RequestEntry
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		var raw rawRequestEntry
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("parse JSONL trace at line %d: %w", lineNum, err)
		}
		entry, err := raw.normalize(len(entries))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum, err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan JSONL trace: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("trace contains no request entries")
	}
	return entries, nil
}

func parseHAR(data []byte) ([]RequestEntry, error) {
	var archive harArchive
	if err := json.Unmarshal(data, &archive); err != nil {
		return nil, fmt.Errorf("parse HAR archive: %w", err)
	}
	if archive.Log == nil || len(archive.Log.Entries) == 0 {
		return nil, errors.New("HAR archive contains no log.entries")
	}

	entries := make([]RequestEntry, 0, len(archive.Log.Entries))
	for i, item := range archive.Log.Entries {
		targetURL := strings.TrimSpace(item.Request.URL)
		if targetURL == "" {
			return nil, fmt.Errorf("HAR entry %d is missing request.url", i)
		}
		method := strings.ToUpper(strings.TrimSpace(item.Request.Method))
		if method == "" {
			method = "GET"
		}

		headers := make(map[string]string)
		hasContentType := false
		for _, h := range item.Request.Headers {
			name := strings.TrimSpace(h.Name)
			if name == "" || strings.HasPrefix(name, ":") {
				continue
			}
			lower := strings.ToLower(name)
			if lower == "host" || lower == "content-length" || lower == "connection" {
				continue
			}
			if lower == "content-type" {
				hasContentType = true
			}
			headers[name] = h.Value
		}

		var body, bodyB64 string
		if pd := item.Request.PostData; pd != nil {
			if !hasContentType && strings.TrimSpace(pd.MimeType) != "" {
				headers["Content-Type"] = strings.TrimSpace(pd.MimeType)
			}
			if strings.EqualFold(strings.TrimSpace(pd.Encoding), "base64") {
				bodyB64 = strings.TrimSpace(pd.Text)
			} else {
				body = pd.Text
			}
		}

		if len(headers) == 0 {
			headers = nil
		}

		entries = append(entries, RequestEntry{
			ID:         fmt.Sprintf("har-%d", i+1),
			Method:     method,
			Path:       targetURL,
			Headers:    headers,
			Body:       body,
			BodyBase64: bodyB64,
		})
	}
	return entries, nil
}
