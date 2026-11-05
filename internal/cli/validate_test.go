package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
	"sigs.k8s.io/yaml"
)

func TestValidateValidConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".diverge.yaml")
	// editorconfig-checker-disable
	err := os.WriteFile(configPath, []byte(`version: "1"
services:
  api:
    paths: ["src/**"]
    image:
      repository: "registry.example.com/api"
`), 0644)
	// editorconfig-checker-enable
	require.NoError(t, err)

	// Change to temp dir so validate finds .diverge.yaml
	origDir, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	app := &App{}
	cmd := newValidateCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)

	err = cmd.RunE(cmd, nil)
	assert.NoError(t, err)
	assert.Contains(t, out.String(), "Config is valid")
}

func TestValidateInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".diverge.yaml")
	err := os.WriteFile(configPath, []byte(`version: 999
`), 0644)
	require.NoError(t, err)

	origDir, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	app := &App{}
	cmd := newValidateCmd(app)
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)

	err = cmd.RunE(cmd, nil)
	// Should return error, NOT call os.Exit
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "config validation failed")
	assert.Contains(t, errBuf.String(), "Config is invalid")
}

func TestValidateMissingConfig(t *testing.T) {
	dir := t.TempDir()

	origDir, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	app := &App{}
	cmd := newValidateCmd(app)

	err := cmd.RunE(cmd, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no .diverge.yaml found")
}

func TestValidateWorksFromAnyDirectory(t *testing.T) {
	// This test verifies the schema is embedded (not loaded from relative path).
	// If the schema were loaded from file://config/schema/..., this would fail
	// when running from a temp directory.
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".diverge.yaml")
	err := os.WriteFile(configPath, []byte(`version: "1"
`), 0644)
	require.NoError(t, err)

	origDir, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	app := &App{}
	cmd := newValidateCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)

	err = cmd.RunE(cmd, nil)
	assert.NoError(t, err, "validate should work from any directory with embedded schema")
}

func TestValidateRemoteConfig(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// editorconfig-checker-disable
		_, _ = w.Write([]byte(`version: "1"
services:
  api:
    paths: ["src/**"]
    image:
      repository: "registry.example.com/api"`))
		// editorconfig-checker-enable
	}))
	defer ts.Close()

	app := &App{}
	cmd := newValidateCmd(app)
	var out bytes.Buffer
	cmd.SetOut(&out)

	cmd.SetArgs([]string{"--config", ts.URL})
	err := cmd.Execute()
	assert.NoError(t, err)
	assert.Contains(t, out.String(), "Config is valid")
}

func TestValidateInvalidConfig_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".diverge.yaml")
	err := os.WriteFile(configPath, []byte(`version: 999`), 0644)
	require.NoError(t, err)

	app := &App{}
	cmd := newValidateCmd(app)
	cmd.SetArgs([]string{"--config", configPath, "--output", "json"})
	var out bytes.Buffer
	cmd.SetOut(&out)

	err = cmd.Execute()
	assert.Error(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(out.Bytes(), &result))

	valid, ok := result["valid"].(bool)
	require.True(t, ok)
	assert.False(t, valid)

	errorsSlice, ok := result["errors"].([]interface{})
	require.True(t, ok)
	assert.NotEmpty(t, errorsSlice)

	for _, errObj := range errorsSlice {
		errMap := errObj.(map[string]interface{})
		_, hasPath := errMap["path"]
		_, hasMsg := errMap["message"]
		assert.True(t, hasPath, "error should have path")
		assert.True(t, hasMsg, "error should have message")
	}
}

func TestValidateValidConfig_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".diverge.yaml")
	// editorconfig-checker-disable
	err := os.WriteFile(configPath, []byte(`version: "1"
services:
  api:
    paths: ["src/**"]
    image:
      repository: "registry.example.com/api"
`), 0644)
	// editorconfig-checker-enable
	require.NoError(t, err)

	app := &App{}
	cmd := newValidateCmd(app)
	cmd.SetArgs([]string{"--config", configPath, "--output", "json"})
	var out bytes.Buffer
	cmd.SetOut(&out)

	err = cmd.Execute()
	assert.NoError(t, err)

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(out.Bytes(), &result))

	valid, ok := result["valid"].(bool)
	require.True(t, ok)
	assert.True(t, valid)

	errorsSlice, ok := result["errors"].([]interface{})
	require.True(t, ok)
	assert.Empty(t, errorsSlice)
}

func TestValidatePrintSchema(t *testing.T) {
	dir := t.TempDir()
	origDir, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	app := &App{}
	cmd := newValidateCmd(app)
	cmd.SetArgs([]string{"--print-schema"})
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := cmd.Execute()
	require.NoError(t, err, "--print-schema should succeed even when .diverge.yaml does not exist")

	var schema map[string]interface{}
	require.NoError(t, json.Unmarshal(out.Bytes(), &schema))
	assert.Equal(t, "http://json-schema.org/draft-07/schema#", schema["$schema"])
	assert.Equal(t, "Diverge Config", schema["title"])
}

func TestValidateDocsAndExampleConfigs(t *testing.T) {
	schemaBytes, err := schemaFS.ReadFile("schema/diverge-config.schema.json")
	require.NoError(t, err)
	schemaLoader := gojsonschema.NewBytesLoader(schemaBytes)

	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	demoConfig := filepath.Join(repoRoot, "examples", "fullstack-demo", ".diverge.yaml")
	demoData, err := os.ReadFile(demoConfig)
	require.NoError(t, err)
	demoJSON, err := yaml.YAMLToJSON(demoData)
	require.NoError(t, err)
	res, err := gojsonschema.Validate(schemaLoader, gojsonschema.NewBytesLoader(demoJSON))
	require.NoError(t, err)
	assert.True(t, res.Valid(), "examples/fullstack-demo/.diverge.yaml failed schema validation: %v", res.Errors())

	var mdFiles []string
	mdFiles = append(mdFiles, filepath.Join(repoRoot, "README.md"))
	err = filepath.Walk(filepath.Join(repoRoot, "docs"), func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(path, ".md") {
			mdFiles = append(mdFiles, path)
		}
		return nil
	})
	require.NoError(t, err)

	checkedBlocks := 0
	for _, mdFile := range mdFiles {
		raw, err := os.ReadFile(mdFile)
		require.NoError(t, err)
		blocks := extractDivergeConfigYAMLBlocks(string(raw))
		for idx, block := range blocks {
			checkedBlocks++
			jsonData, err := yaml.YAMLToJSON([]byte(block))
			require.NoErrorf(t, err, "%s (block %d) invalid YAML", mdFile, idx+1)
			result, err := gojsonschema.Validate(schemaLoader, gojsonschema.NewBytesLoader(jsonData))
			require.NoErrorf(t, err, "%s (block %d) validation error", mdFile, idx+1)
			if !result.Valid() {
				var errMsgs []string
				for _, e := range result.Errors() {
					errMsgs = append(errMsgs, e.String())
				}
				t.Errorf("%s (block %d) failed .diverge.yaml schema validation:\n%s", mdFile, idx+1, strings.Join(errMsgs, "\n"))
			}
		}
	}
	assert.Greater(t, checkedBlocks, 0, "expected to validate at least one .diverge.yaml block in docs/")
}

func extractDivergeConfigYAMLBlocks(markdown string) []string {
	var blocks []string
	lines := strings.Split(markdown, "\n")
	inYAML := false
	var current []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inYAML {
			if trimmed == "```yaml" || trimmed == "```yml" {
				inYAML = true
				current = nil
			}
			continue
		}
		if trimmed == "```" {
			inYAML = false
			block := strings.Join(current, "\n")
			if strings.Contains(block, `version: "1"`) && strings.Contains(block, "\nservices:") {
				blocks = append(blocks, block)
			}
			continue
		}
		current = append(current, line)
	}
	return blocks
}
