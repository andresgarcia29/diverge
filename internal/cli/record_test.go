package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/divergedev/diverge/pkg/replay"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRecordCmd_CapturesAndSanitizesRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer upstream.Close()

	outFile := filepath.Join(t.TempDir(), "captured.jsonl")
	app := &App{}
	root := NewRootCmd(app)

	var stdout, stderr syncBuffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{
		"record",
		"--target", upstream.URL,
		"--listen", "127.0.0.1:0",
		"--output", outFile,
		"--max-requests", "1",
		"--duration", "5s",
		"--redact-query", "custom_tok",
		"--redact-body-field", "secret_pin",
	})

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- root.Execute()
	}()

	// Wait until the proxy prints its listening address to stderr
	var proxyURL string
	re := regexp.MustCompile(`http://127\.0\.0\.1:\d+`)
	require.Eventually(t, func() bool {
		m := re.FindString(stderr.String())
		if m != "" {
			proxyURL = m
			return true
		}
		return false
	}, 3*time.Second, 20*time.Millisecond)

	req, err := http.NewRequest("POST", proxyURL+"/api/v1/pay?token=jwt-123&custom_tok=ct-9&page=1", strings.NewReader(`{"amount":42,"secret_pin":"1234"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer top-secret")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	require.NoError(t, <-doneCh)
	assert.Contains(t, stdout.String(), "Recorded 1 sanitized request(s)")

	entries, err := replay.LoadTrace(outFile)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "POST", entries[0].Method)
	assert.NotContains(t, entries[0].Path, "jwt-123")
	assert.NotContains(t, entries[0].Path, "ct-9")
	assert.Contains(t, entries[0].Path, "page=1")
	assert.NotContains(t, entries[0].Headers, "Authorization")
	assert.Contains(t, entries[0].Body, `"secret_pin":"[REDACTED]"`)
	assert.NotContains(t, entries[0].Body, "1234")

	// Missing --target without env arg returns an error
	rootMissing := NewRootCmd(app)
	rootMissing.SetOut(&bytes.Buffer{})
	rootMissing.SetErr(&bytes.Buffer{})
	rootMissing.SetArgs([]string{"record", "--output", filepath.Join(t.TempDir(), "unused.jsonl")})
	err = rootMissing.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--target")
	_ = os.Remove(outFile)
}
