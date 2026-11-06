package cli

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/protocgen/proto2mcp/pkg/mcpruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	divergev1alpha1 "github.com/divergedev/diverge/api/gen/diverge/v1alpha1"
)

type mockPgClient struct{}

func (m *mockPgClient) CreatePreviewGroup(ctx context.Context, req *connect.Request[divergev1alpha1.CreatePreviewGroupRequest]) (*connect.Response[divergev1alpha1.CreatePreviewGroupResponse], error) {
	return nil, nil
}
func (m *mockPgClient) GetPreviewGroup(ctx context.Context, req *connect.Request[divergev1alpha1.GetPreviewGroupRequest]) (*connect.Response[divergev1alpha1.GetPreviewGroupResponse], error) {
	return nil, nil
}
func (m *mockPgClient) ListPreviewGroups(ctx context.Context, req *connect.Request[divergev1alpha1.ListPreviewGroupsRequest]) (*connect.Response[divergev1alpha1.ListPreviewGroupsResponse], error) {
	return nil, nil
}
func (m *mockPgClient) UpdatePreviewGroup(ctx context.Context, req *connect.Request[divergev1alpha1.UpdatePreviewGroupRequest]) (*connect.Response[divergev1alpha1.UpdatePreviewGroupResponse], error) {
	return nil, nil
}
func (m *mockPgClient) DeletePreviewGroup(ctx context.Context, req *connect.Request[divergev1alpha1.DeletePreviewGroupRequest]) (*connect.Response[divergev1alpha1.DeletePreviewGroupResponse], error) {
	return nil, nil
}
func (m *mockPgClient) WatchPreviewGroups(ctx context.Context, req *connect.Request[divergev1alpha1.WatchPreviewGroupsRequest]) (*connect.ServerStreamForClient[divergev1alpha1.WatchPreviewGroupsResponse], error) {
	return nil, nil
}

type mockTaskClient struct {
	err error
}

func (m *mockTaskClient) CreateTask(ctx context.Context, req *connect.Request[divergev1alpha1.CreateTaskRequest]) (*connect.Response[divergev1alpha1.CreateTaskResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&divergev1alpha1.CreateTaskResponse{
		Task: &divergev1alpha1.AgentTask{Name: req.Msg.Name, Namespace: req.Msg.Namespace},
	}), nil
}
func (m *mockTaskClient) GetTask(ctx context.Context, req *connect.Request[divergev1alpha1.GetTaskRequest]) (*connect.Response[divergev1alpha1.GetTaskResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&divergev1alpha1.GetTaskResponse{
		Task: &divergev1alpha1.AgentTask{Name: req.Msg.Name, Namespace: req.Msg.Namespace},
	}), nil
}
func (m *mockTaskClient) ListTasks(ctx context.Context, req *connect.Request[divergev1alpha1.ListTasksRequest]) (*connect.Response[divergev1alpha1.ListTasksResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&divergev1alpha1.ListTasksResponse{}), nil
}
func (m *mockTaskClient) DeleteTask(ctx context.Context, req *connect.Request[divergev1alpha1.DeleteTaskRequest]) (*connect.Response[divergev1alpha1.DeleteTaskResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&divergev1alpha1.DeleteTaskResponse{}), nil
}
func (m *mockTaskClient) PauseTask(ctx context.Context, req *connect.Request[divergev1alpha1.PauseTaskRequest]) (*connect.Response[divergev1alpha1.PauseTaskResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&divergev1alpha1.PauseTaskResponse{
		Task: &divergev1alpha1.AgentTask{Name: req.Msg.Name, Namespace: req.Msg.Namespace},
	}), nil
}
func (m *mockTaskClient) ResumeTask(ctx context.Context, req *connect.Request[divergev1alpha1.ResumeTaskRequest]) (*connect.Response[divergev1alpha1.ResumeTaskResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&divergev1alpha1.ResumeTaskResponse{
		Task: &divergev1alpha1.AgentTask{Name: req.Msg.Name, Namespace: req.Msg.Namespace},
	}), nil
}
func (m *mockTaskClient) GuideTask(ctx context.Context, req *connect.Request[divergev1alpha1.GuideTaskRequest]) (*connect.Response[divergev1alpha1.GuideTaskResponse], error) {
	if m.err != nil {
		return nil, m.err
	}
	return connect.NewResponse(&divergev1alpha1.GuideTaskResponse{}), nil
}
func (m *mockTaskClient) StreamTaskLogs(ctx context.Context, req *connect.Request[divergev1alpha1.StreamTaskLogsRequest]) (*connect.ServerStreamForClient[divergev1alpha1.StreamTaskLogsResponse], error) {
	return nil, m.err
}

func TestPascalToSnake(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"GetEnvironment", "get_environment"},
		{"CreatePreviewGroup", "create_preview_group"},
		{"WatchEnvironments", "watch_environments"},
		{"StreamLogs", "stream_logs"},
		{"ABCTest", "abc_test"},
		{"ExtendTTL", "extend_ttl"},
		{"ListHTTPRoutes", "list_http_routes"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, pascalToSnake(tt.input))
		})
	}
}

func TestDivergeToolNamer(t *testing.T) {
	assert.Equal(t, "diverge_get_environment", divergeToolNamer("EnvironmentService", "GetEnvironment"))
	assert.Equal(t, "diverge_extend_ttl", divergeToolNamer("EnvironmentService", "ExtendTTL"))
	assert.Equal(t, "diverge_create_preview_group", divergeToolNamer("PreviewGroupService", "CreatePreviewGroup"))
	assert.Equal(t, "diverge_create_task", divergeToolNamer("AgentTaskService", "CreateTask"))
}

func TestMCPToolRegistration(t *testing.T) {
	server := newMCPServer(&mockEnvClient{}, &mockPgClient{}, &mockTaskClient{}, true)
	require.NotNil(t, server)

	tools := server.ListTools()

	// Verify all expected EnvironmentService tools are registered
	expectedEnvTools := []string{
		"diverge_create_environment",
		"diverge_get_environment",
		"diverge_list_environments",
		"diverge_update_environment",
		"diverge_delete_environment",
		divergeToolNamer("EnvironmentService", "ExtendTTL"),
		"diverge_list_hook_jobs",
		"diverge_retry_hook",
		"diverge_wait_for_ready",
		"diverge_fetch_errors",
		"diverge_visual_test",
		"diverge_replay",
	}
	for _, name := range expectedEnvTools {
		assert.Contains(t, tools, name, "expected EnvironmentService tool %s to be registered", name)
	}

	// Verify all expected PreviewGroupService tools are registered
	expectedPgTools := []string{
		"diverge_create_preview_group",
		"diverge_get_preview_group",
		"diverge_list_preview_groups",
		"diverge_update_preview_group",
		"diverge_delete_preview_group",
	}
	for _, name := range expectedPgTools {
		assert.Contains(t, tools, name, "expected PreviewGroupService tool %s to be registered", name)
	}

	// Verify all expected AgentTaskService tools are registered with descriptions and side-effect annotations
	expectedTaskTools := map[string]struct {
		readOnly    bool
		destructive bool
	}{
		"diverge_create_task": {readOnly: false, destructive: false},
		"diverge_get_task":    {readOnly: true, destructive: false},
		"diverge_list_tasks":  {readOnly: true, destructive: false},
		"diverge_delete_task": {readOnly: false, destructive: true},
		"diverge_pause_task":  {readOnly: false, destructive: false},
		"diverge_resume_task": {readOnly: false, destructive: false},
		"diverge_guide_task":  {readOnly: false, destructive: false},
	}
	for name, expected := range expectedTaskTools {
		serverTool, ok := tools[name]
		if assert.True(t, ok, "expected AgentTaskService tool %s to be registered", name) {
			assert.NotEmpty(t, serverTool.Tool.Description, "tool %s must have a non-empty description", name)
			require.NotNil(t, serverTool.Tool.Annotations.ReadOnlyHint, "tool %s must set ReadOnlyHint", name)
			assert.Equal(t, expected.readOnly, *serverTool.Tool.Annotations.ReadOnlyHint, "unexpected ReadOnlyHint for %s", name)
			require.NotNil(t, serverTool.Tool.Annotations.DestructiveHint, "tool %s must set DestructiveHint", name)
			assert.Equal(t, expected.destructive, *serverTool.Tool.Annotations.DestructiveHint, "unexpected DestructiveHint for %s", name)
		}
	}

	// Verify Auth, Cluster, and Tunnel services are NOT registered
	unregisteredSubstrings := []string{"auth", "login", "logout", "token", "cluster", "tunnel"}
	for name := range tools {
		for _, substr := range unregisteredSubstrings {
			assert.False(t, strings.Contains(name, substr), "tool %s should not belong to Auth, Cluster, or Tunnel service", name)
		}
	}

	// Verify streaming methods (WatchEnvironments, StreamLogs, WatchPreviewGroups, StreamTaskLogs) are NOT registered
	streamingToolNames := []string{
		"diverge_watch_environments",
		"diverge_stream_logs",
		"diverge_watch_preview_groups",
		"diverge_stream_task_logs",
	}
	for _, name := range streamingToolNames {
		assert.NotContains(t, tools, name, "streaming method %s must not be registered as an MCP tool", name)
	}
}

func TestMCPDestructiveFiltering(t *testing.T) {
	t.Run("without allow-destructive", func(t *testing.T) {
		server := newMCPServer(&mockEnvClient{}, &mockPgClient{}, &mockTaskClient{}, false)
		tools := server.ListTools()

		// Verify DeleteEnvironment, DeletePreviewGroup, and DeleteTask are excluded
		assert.NotContains(t, tools, "diverge_delete_environment")
		assert.NotContains(t, tools, "diverge_delete_preview_group")
		assert.NotContains(t, tools, "diverge_delete_task")

		// Verify non-destructive tools are still present
		assert.Contains(t, tools, "diverge_get_environment")
		assert.Contains(t, tools, "diverge_get_preview_group")
		assert.Contains(t, tools, "diverge_get_task")
		assert.Contains(t, tools, "diverge_create_environment")
		assert.Contains(t, tools, "diverge_create_preview_group")
		assert.Contains(t, tools, "diverge_create_task")
	})

	t.Run("with allow-destructive", func(t *testing.T) {
		server := newMCPServer(&mockEnvClient{}, &mockPgClient{}, &mockTaskClient{}, true)
		tools := server.ListTools()

		// Verify DeleteEnvironment, DeletePreviewGroup, and DeleteTask are included
		assert.Contains(t, tools, "diverge_delete_environment")
		assert.Contains(t, tools, "diverge_delete_preview_group")
		assert.Contains(t, tools, "diverge_delete_task")
	})
}

func TestMCPToolNaming(t *testing.T) {
	// Specific examples from spec
	assert.Equal(t, "diverge_create_environment", divergeToolNamer("EnvironmentService", "CreateEnvironment"))
	assert.Equal(t, "diverge_get_preview_group", divergeToolNamer("PreviewGroupService", "GetPreviewGroup"))

	// Test all registered tools follow diverge_<snake_case> convention
	server := newMCPServer(&mockEnvClient{}, &mockPgClient{}, &mockTaskClient{}, true)
	tools := server.ListTools()
	require.NotEmpty(t, tools)

	snakeCasePattern := regexp.MustCompile(`^diverge_[a-z0-9]+(_[a-z0-9]+)*$`)
	for name := range tools {
		assert.True(t, strings.HasPrefix(name, "diverge_"), "tool %s should start with 'diverge_'", name)
		assert.True(t, snakeCasePattern.MatchString(name), "tool %s should follow diverge_<snake_case> convention", name)
	}
}

func TestMCPTaskHandler(t *testing.T) {
	ctx := context.Background()
	h := &mcpTaskHandler{client: &mockTaskClient{}}

	createResp, err := h.CreateTask(ctx, &divergev1alpha1.CreateTaskRequest{Name: "t1", Namespace: "ns1"})
	require.NoError(t, err)
	assert.Equal(t, "t1", createResp.Task.Name)

	getResp, err := h.GetTask(ctx, &divergev1alpha1.GetTaskRequest{Name: "t1", Namespace: "ns1"})
	require.NoError(t, err)
	assert.Equal(t, "t1", getResp.Task.Name)

	_, err = h.ListTasks(ctx, &divergev1alpha1.ListTasksRequest{Namespace: "ns1"})
	require.NoError(t, err)

	_, err = h.DeleteTask(ctx, &divergev1alpha1.DeleteTaskRequest{Name: "t1", Namespace: "ns1"})
	require.NoError(t, err)

	pauseResp, err := h.PauseTask(ctx, &divergev1alpha1.PauseTaskRequest{Name: "t1", Namespace: "ns1"})
	require.NoError(t, err)
	assert.Equal(t, "t1", pauseResp.Task.Name)

	resumeResp, err := h.ResumeTask(ctx, &divergev1alpha1.ResumeTaskRequest{Name: "t1", Namespace: "ns1"})
	require.NoError(t, err)
	assert.Equal(t, "t1", resumeResp.Task.Name)

	_, err = h.GuideTask(ctx, &divergev1alpha1.GuideTaskRequest{Name: "t1", Namespace: "ns1", Message: "focus on tests"})
	require.NoError(t, err)

	// Verify error propagation
	errHandler := &mcpTaskHandler{client: &mockTaskClient{err: assert.AnError}}
	_, err = errHandler.CreateTask(ctx, &divergev1alpha1.CreateTaskRequest{})
	assert.ErrorIs(t, err, assert.AnError)
	_, err = errHandler.GetTask(ctx, &divergev1alpha1.GetTaskRequest{})
	assert.ErrorIs(t, err, assert.AnError)
	_, err = errHandler.ListTasks(ctx, &divergev1alpha1.ListTasksRequest{})
	assert.ErrorIs(t, err, assert.AnError)
	_, err = errHandler.DeleteTask(ctx, &divergev1alpha1.DeleteTaskRequest{})
	assert.ErrorIs(t, err, assert.AnError)
	_, err = errHandler.PauseTask(ctx, &divergev1alpha1.PauseTaskRequest{})
	assert.ErrorIs(t, err, assert.AnError)
	_, err = errHandler.ResumeTask(ctx, &divergev1alpha1.ResumeTaskRequest{})
	assert.ErrorIs(t, err, assert.AnError)
	_, err = errHandler.GuideTask(ctx, &divergev1alpha1.GuideTaskRequest{})
	assert.ErrorIs(t, err, assert.AnError)
}

func TestMCPBearerTokenAuth(t *testing.T) {
	tmpDir := t.TempDir()
	tokenFile := filepath.Join(tmpDir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("  file-macaroon-token-1\n"), 0o600))

	t.Setenv("DIVERGE_TOKEN", "")
	t.Setenv("DIVERGE_MACAROON_TOKEN", "")
	t.Setenv("DIVERGE_TOKEN_FILE", tokenFile)
	assert.Equal(t, "file-macaroon-token-1", resolveMCPBearerToken())

	var seenAuthHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := newMCPHTTPClient(ts.URL)
	resp, err := client.Get(ts.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, "Bearer file-macaroon-token-1", seenAuthHeader)

	// Rotating the token file on disk (e.g., after ResumeTask re-mints Secret) is picked up dynamically
	require.NoError(t, os.WriteFile(tokenFile, []byte("file-macaroon-token-2"), 0o600))
	resp, err = client.Get(ts.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, "Bearer file-macaroon-token-2", seenAuthHeader)

	// Explicit DIVERGE_TOKEN takes precedence over DIVERGE_TOKEN_FILE
	t.Setenv("DIVERGE_TOKEN", "env-override-token")
	resp, err = client.Get(ts.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, "Bearer env-override-token", seenAuthHeader)

	// Cross-origin redirect must NOT receive the Authorization bearer token
	var secondOriginAuthHeader string
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondOriginAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer secondServer.Close()

	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, secondServer.URL, http.StatusFound)
	}))
	defer redirectServer.Close()

	redirectClient := newMCPHTTPClient(redirectServer.URL)
	resp, err = redirectClient.Get(redirectServer.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Empty(t, secondOriginAuthHeader, "cross-origin redirect target must not receive Authorization header")

	// Non-loopback plaintext HTTP must be rejected before sending credentials
	remoteHTTPClient := newMCPHTTPClient("http://diverge.example.com:8080")
	_, err = remoteHTTPClient.Get("http://diverge.example.com:8080/healthz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "insecure HTTP scheme is only allowed for loopback addresses")

	// In-cluster Kubernetes Service DNS hosts (.svc and .svc.cluster.local) are allowed over HTTP
	assert.True(t, isLoopbackOrClusterHost("diverge-server.diverge-system.svc:8080"))
	assert.True(t, isLoopbackOrClusterHost("diverge-server.diverge-system.svc.cluster.local:8443"))
	assert.False(t, isLoopbackOrClusterHost("diverge.example.com:8080"))

	// Verify resolveMCPServerURL falls back to DIVERGE_MCP_URL and strips /mcp suffix
	t.Setenv("DIVERGE_SERVER_URL", "")
	t.Setenv("DIVERGE_MCP_URL", "http://diverge-server.diverge-system.svc:8443/mcp")
	assert.Equal(t, "http://diverge-server.diverge-system.svc:8443", resolveMCPServerURL(""))
}

func TestMCPCmdVisibleInHelp(t *testing.T) {
	cmd := newMCPCmd(&App{})
	assert.False(t, cmd.Hidden, "diverge mcp command must not be hidden in --help")

	var names []string
	for _, def := range customTools() {
		names = append(names, def.Name)
	}
	assert.Contains(t, names, "diverge_visual_test")
}

func TestMCPVisualTestTool(t *testing.T) {
	registry := mcpruntime.NewToolRegistry()
	registerVisualTest(registry)

	handler, ok := registry.Lookup("diverge_visual_test")
	require.True(t, ok)

	tmpDir := t.TempDir()
	baseDir := filepath.Join(tmpDir, "baseline")
	prevDir := filepath.Join(tmpDir, "preview")
	diffDir := filepath.Join(tmpDir, "diffs")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	require.NoError(t, os.MkdirAll(prevDir, 0o755))

	white := color.RGBA{255, 255, 255, 255}
	red := color.RGBA{255, 0, 0, 255}

	baseImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	draw.Draw(baseImg, baseImg.Bounds(), &image.Uniform{C: white}, image.Point{}, draw.Src)
	basePath := filepath.Join(baseDir, "home.png")
	writeTestPNG(t, basePath, baseImg)

	// Identical preview image
	prevSamePath := filepath.Join(prevDir, "home.png")
	writeTestPNG(t, prevSamePath, baseImg)

	// 1. Single-file pass with diff output
	singleDiffPath := filepath.Join(diffDir, "single-diff.png")
	reqArgs, _ := json.Marshal(map[string]any{
		"baseline_path":    basePath,
		"preview_path":     prevSamePath,
		"diff_output_path": singleDiffPath,
	})
	res, err := handler(context.Background(), mcpruntime.ToolRequest{
		ToolName:  "diverge_visual_test",
		Arguments: reqArgs,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	assert.FileExists(t, singleDiffPath)

	var fileRes visualTestFileResult
	require.NoError(t, json.Unmarshal(res.Content, &fileRes))
	assert.True(t, fileRes.Passed)
	assert.Equal(t, 0.0, fileRes.DiffPercent)
	assert.Equal(t, 0.1, fileRes.MaxDiffPercent)

	// 2. Single-file drift exceeding default 0.1% threshold -> IsError: true
	driftedImg := image.NewRGBA(image.Rect(0, 0, 100, 100))
	draw.Draw(driftedImg, driftedImg.Bounds(), &image.Uniform{C: white}, image.Point{}, draw.Src)
	draw.Draw(driftedImg, image.Rect(0, 0, 10, 10), &image.Uniform{C: red}, image.Point{}, draw.Src) // 1% drift
	driftedPath := filepath.Join(tmpDir, "drifted.png")
	writeTestPNG(t, driftedPath, driftedImg)

	driftArgs, _ := json.Marshal(map[string]any{
		"baseline_path": basePath,
		"preview_path":  driftedPath,
	})
	driftRes, err := handler(context.Background(), mcpruntime.ToolRequest{
		ToolName:  "diverge_visual_test",
		Arguments: driftArgs,
	})
	require.NoError(t, err)
	require.NotNil(t, driftRes)
	assert.True(t, driftRes.IsError)

	// 3. Directory comparison mode
	dirArgs, _ := json.Marshal(map[string]any{
		"baseline_dir":     baseDir,
		"preview_dir":      prevDir,
		"diff_output_path": filepath.Join(diffDir, "batch"),
	})
	dirToolRes, err := handler(context.Background(), mcpruntime.ToolRequest{
		ToolName:  "diverge_visual_test",
		Arguments: dirArgs,
	})
	require.NoError(t, err)
	require.NotNil(t, dirToolRes)
	assert.False(t, dirToolRes.IsError)

	var batchRes visualTestDirResult
	require.NoError(t, json.Unmarshal(dirToolRes.Content, &batchRes))
	assert.True(t, batchRes.Passed)
	assert.Equal(t, 1, batchRes.TotalFiles)
	assert.Equal(t, 0, batchRes.FailedFiles)
	assert.FileExists(t, filepath.Join(diffDir, "batch", "home.png"))
}
