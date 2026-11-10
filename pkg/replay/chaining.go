package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// chainingStore holds isolated variable maps extracted from baseline and preview responses across sequential steps.
type chainingStore struct {
	mu       sync.RWMutex
	baseVars map[string]string
	prevVars map[string]string
}

// newChainingStore constructs an empty chainingStore for baseline and preview variable tracking.
func newChainingStore() *chainingStore {
	return &chainingStore{
		baseVars: make(map[string]string),
		prevVars: make(map[string]string),
	}
}

// snapshot returns shallow copies of the current baseline and preview variable maps.
func (s *chainingStore) snapshot() (map[string]string, map[string]string) {
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b := make(map[string]string, len(s.baseVars))
	for k, v := range s.baseVars {
		b[k] = v
	}
	p := make(map[string]string, len(s.prevVars))
	for k, v := range s.prevVars {
		p[k] = v
	}
	return b, p
}

// merge stores newly extracted baseline and preview variables into the store.
func (s *chainingStore) merge(baseExtracted, prevExtracted map[string]string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range baseExtracted {
		s.baseVars[k] = v
	}
	for k, v := range prevExtracted {
		s.prevVars[k] = v
	}
}

// HasChaining reports whether any entry in entries defines variable extractions or references {{var}} placeholders.
func HasChaining(entries []RequestEntry) bool {
	for _, e := range entries {
		if len(e.Extract) > 0 {
			return true
		}
		if containsTemplatePlaceholder(e.Path) || containsTemplatePlaceholder(e.URL) || containsTemplatePlaceholder(e.Body) {
			return true
		}
		for _, v := range e.Headers {
			if containsTemplatePlaceholder(v) {
				return true
			}
		}
	}
	return false
}

// containsTemplatePlaceholder reports whether s contains a {{...}} variable placeholder.
func containsTemplatePlaceholder(s string) bool {
	openIdx := strings.Index(s, "{{")
	if openIdx < 0 {
		return false
	}
	closeIdx := strings.Index(s[openIdx+2:], "}}")
	return closeIdx > 0
}

// InterpolateString replaces {{varName}} placeholders in s using values from vars, leaving unknown placeholders unchanged.
func InterpolateString(s string, vars map[string]string) string {
	if s == "" || len(vars) == 0 || !strings.Contains(s, "{{") {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	rem := s
	for {
		openIdx := strings.Index(rem, "{{")
		if openIdx < 0 {
			out.WriteString(rem)
			break
		}
		out.WriteString(rem[:openIdx])
		afterOpen := rem[openIdx+2:]
		closeIdx := strings.Index(afterOpen, "}}")
		if closeIdx < 0 {
			out.WriteString(rem[openIdx:])
			break
		}
		varName := strings.TrimSpace(afterOpen[:closeIdx])
		if val, ok := vars[varName]; ok {
			out.WriteString(val)
		} else {
			out.WriteString(rem[openIdx : openIdx+2+closeIdx+2])
		}
		rem = afterOpen[closeIdx+2:]
	}
	return out.String()
}

// InterpolateHeaders returns a copy of headers with {{varName}} placeholders interpolated from vars.
func InterpolateHeaders(headers map[string]string, vars map[string]string) map[string]string {
	if len(headers) == 0 || len(vars) == 0 {
		return headers
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		out[k] = InterpolateString(v, vars)
	}
	return out
}

// InterpolateBytes replaces {{varName}} placeholders in payload using vars when payload contains template tokens.
func InterpolateBytes(payload []byte, vars map[string]string) []byte {
	if len(payload) == 0 || len(vars) == 0 || !bytes.Contains(payload, []byte("{{")) {
		return payload
	}
	return []byte(InterpolateString(string(payload), vars))
}

// ExtractResponseVars evaluates each variable extraction selector in rules against the HTTP response status, headers, and body.
// Supported selectors:
//   - "$status" -> HTTP status code string (e.g. "201")
//   - "$header.<Name>" or "header:<Name>" -> HTTP response header value
//   - "$.path.to.field[0]" or "path.to.field" -> JSON response body value
func ExtractResponseVars(rules map[string]string, status int, headers http.Header, body []byte) (map[string]string, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(rules))
	var parsedJSON any
	jsonDecoded := false

	for varName, rawSelector := range rules {
		name := strings.TrimSpace(varName)
		selector := strings.TrimSpace(rawSelector)
		if name == "" || selector == "" {
			continue
		}

		if strings.EqualFold(selector, "$status") {
			out[name] = strconv.Itoa(status)
			continue
		}

		if hdrName, ok := parseHeaderSelector(selector); ok {
			if headers == nil {
				return nil, fmt.Errorf("variable %q: response header %q not found", name, hdrName)
			}
			val := strings.TrimSpace(headers.Get(hdrName))
			if val == "" {
				return nil, fmt.Errorf("variable %q: response header %q is empty", name, hdrName)
			}
			out[name] = val
			continue
		}

		if !jsonDecoded {
			trimmed := bytes.TrimSpace(body)
			if len(trimmed) == 0 {
				return nil, fmt.Errorf("variable %q: cannot extract JSON path %q from empty response body", name, selector)
			}
			dec := json.NewDecoder(bytes.NewReader(trimmed))
			dec.UseNumber()
			if err := dec.Decode(&parsedJSON); err != nil {
				return nil, fmt.Errorf("variable %q: decode JSON response for %q: %w", name, selector, err)
			}
			jsonDecoded = true
		}

		val, err := lookupJSONSelector(parsedJSON, selector)
		if err != nil {
			return nil, fmt.Errorf("variable %q (%s): %w", name, selector, err)
		}
		out[name] = val
	}
	return out, nil
}

// parseHeaderSelector checks whether selector is a "$header.<Name>" or "header:<Name>" expression and returns the header name.
func parseHeaderSelector(selector string) (string, bool) {
	lower := strings.ToLower(selector)
	if strings.HasPrefix(lower, "$header.") {
		h := strings.TrimSpace(selector[len("$header."):])
		return h, h != ""
	}
	if strings.HasPrefix(lower, "header:") {
		h := strings.TrimSpace(selector[len("header:"):])
		return h, h != ""
	}
	return "", false
}

// lookupJSONSelector navigates a decoded JSON value using a dot/bracket path such as "$.order.items[0].id".
func lookupJSONSelector(root any, selector string) (string, error) {
	path := strings.TrimSpace(selector)
	path = strings.TrimPrefix(path, "$.")
	path = strings.TrimPrefix(path, "$")
	path = strings.TrimPrefix(path, ".")

	cur := root
	if path != "" {
		segments := strings.Split(path, ".")
		for _, seg := range segments {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			var err error
			cur, err = stepJSONSegment(cur, seg)
			if err != nil {
				return "", err
			}
		}
	}

	switch v := cur.(type) {
	case nil:
		return "", fmt.Errorf("field is null")
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		return strconv.FormatBool(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// stepJSONSegment resolves a single path segment (optionally containing one or more "[index]" suffixes) against cur.
func stepJSONSegment(cur any, seg string) (any, error) {
	rem := seg
	bracketIdx := strings.IndexByte(rem, '[')
	fieldKey := rem
	if bracketIdx >= 0 {
		fieldKey = rem[:bracketIdx]
		rem = rem[bracketIdx:]
	} else {
		rem = ""
	}

	if fieldKey != "" {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cannot access field %q on non-object", fieldKey)
		}
		next, exists := obj[fieldKey]
		if !exists {
			return nil, fmt.Errorf("field %q not found", fieldKey)
		}
		cur = next
	}

	for strings.HasPrefix(rem, "[") {
		closeIdx := strings.IndexByte(rem, ']')
		if closeIdx < 0 {
			return nil, fmt.Errorf("unclosed bracket in %q", seg)
		}
		idxStr := strings.TrimSpace(rem[1:closeIdx])
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 {
			return nil, fmt.Errorf("invalid array index %q", idxStr)
		}
		arr, ok := cur.([]any)
		if !ok {
			return nil, fmt.Errorf("cannot index non-array with [%d]", idx)
		}
		if idx >= len(arr) {
			return nil, fmt.Errorf("array index %d out of bounds (len %d)", idx, len(arr))
		}
		cur = arr[idx]
		rem = strings.TrimSpace(rem[closeIdx+1:])
	}
	return cur, nil
}
