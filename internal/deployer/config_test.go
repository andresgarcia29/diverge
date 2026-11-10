package deployer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDotDivergeConfig(t *testing.T) {
	// editorconfig-checker-disable
	tests := []struct {
		name     string
		input    string
		wantSvc  string
		wantPort int32
		wantErr  bool
	}{
		{
			name: "valid config",
			input: `
apiVersion: divergedev.com/v1alpha1
kind: ServicePreview
metadata:
  name: payments-api
spec:
  namespace: demo-bank
  serviceName: payments-api
  port: 8080
  routing:
    headerKey: x-preview-id
  container:
    env:
      - name: LOG_LEVEL
        value: debug
`,
			wantSvc:  "payments-api",
			wantPort: 8080,
		},
		{
			name: "default port",
			input: `
apiVersion: divergedev.com/v1alpha1
kind: ServicePreview
metadata:
  name: test-svc
spec:
  serviceName: test-svc
`,
			wantSvc:  "test-svc",
			wantPort: 8080,
		},
		{
			name: "missing serviceName",
			input: `
apiVersion: divergedev.com/v1alpha1
kind: ServicePreview
metadata:
  name: test
spec:
  port: 8080
`,
			wantErr: true,
		},
		{
			name:    "invalid yaml",
			input:   `{{{invalid`,
			wantErr: true,
		},
		{
			name: "invalid port -1",
			input: `
apiVersion: divergedev.com/v1alpha1
kind: ServicePreview
metadata:
  name: test
spec:
  serviceName: test
  port: -1
`,
			wantErr: true,
		},
		{
			name: "invalid port 65536",
			input: `
apiVersion: divergedev.com/v1alpha1
kind: ServicePreview
metadata:
  name: test
spec:
  serviceName: test
  port: 65536
`,
			wantErr: true,
		},
	}
	// editorconfig-checker-enable

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseDotDivergeConfig([]byte(tt.input))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSvc, cfg.Spec.ServiceName)
			assert.Equal(t, tt.wantPort, cfg.Spec.Port)
		})
	}
}

func TestToServicePreviewConfig(t *testing.T) {
	// editorconfig-checker-disable
	cfg, err := ParseDotDivergeConfig([]byte(`
apiVersion: divergedev.com/v1alpha1
kind: ServicePreview
metadata:
  name: payments-api
spec:
  namespace: demo-bank
  serviceName: payments-api
  port: 9090
  routing:
    parentRef: banking-waypoint
    headerKey: x-preview-id
  container:
    env:
      - name: DB_URL
        value: postgres://localhost/preview
`))
	// editorconfig-checker-enable
	require.NoError(t, err)

	spc := cfg.ToServicePreviewConfig("registry/payments-api:mr-42")

	assert.Equal(t, "payments-api", spc.ServiceName)
	assert.Equal(t, int32(9090), spc.Port)
	assert.Equal(t, "registry/payments-api:mr-42", spc.Image)
	assert.Equal(t, "banking-waypoint", spc.ParentRef)
	require.Len(t, spc.Env, 1)
	assert.Equal(t, "DB_URL", spc.Env[0].Name)
	assert.Equal(t, "postgres://localhost/preview", spc.Env[0].Value)
}

func TestToServicePreviewConfig_WebSocket(t *testing.T) {
	// editorconfig-checker-disable
	cfg, err := ParseDotDivergeConfig([]byte(`
apiVersion: divergedev.com/v1alpha1
kind: ServicePreview
metadata:
  name: payments-api
spec:
  namespace: demo-bank
  serviceName: payments-api
  port: 9090
  websocket:
    enabled: true
    path: /ws
    timeout: 3600s
`))
	// editorconfig-checker-enable
	require.NoError(t, err)

	spc := cfg.ToServicePreviewConfig("registry/payments-api:mr-42")

	require.NotNil(t, spc.WebSocket)
	assert.True(t, spc.WebSocket.Enabled)
	assert.Equal(t, "/ws", spc.WebSocket.Path)
	assert.Equal(t, "3600s", spc.WebSocket.Timeout)
}

func TestParseDotDivergeConfig_ImageRepository(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		wantErr string
	}{
		{name: "unset", repo: ""},
		{name: "registry and path", repo: "registry.example.com/team/payments-api"},
		{name: "registry with port", repo: "registry.example.com:5000/team/payments-api"},
		{name: "tag rejected", repo: "registry.example.com/team/payments-api:latest", wantErr: "must not contain a tag"},
		{name: "digest rejected", repo: "registry.example.com/team/payments-api@sha256:abc", wantErr: "must not contain a digest"},
		{name: "whitespace rejected", repo: "registry.example.com/team/payments api", wantErr: "must not contain a digest or whitespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := "spec:\n  serviceName: payments-api\n  image:\n    repository: \"" + tt.repo + "\"\n"
			cfg, err := ParseDotDivergeConfig([]byte(input))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.repo, cfg.Spec.Image.Repository)
		})
	}
}

func TestPreviewImage(t *testing.T) {
	tests := []struct {
		name string
		repo string
		want string
	}{
		{name: "falls back to the Git repository path", repo: "", want: "team/payments-api:abc123def456"},
		{name: "uses the configured repository", repo: "registry.example.com/previews/payments-api", want: "registry.example.com/previews/payments-api:abc123def456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &DotDivergeConfig{}
			cfg.Spec.Image.Repository = tt.repo
			assert.Equal(t, tt.want, cfg.PreviewImage("team/payments-api", "abc123def456"))
		})
	}
}
