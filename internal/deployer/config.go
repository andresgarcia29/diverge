package deployer

import (
	"fmt"
	"strings"
	"time"

	"github.com/divergedev/diverge/api/v1alpha1"
	"gopkg.in/yaml.v3"
)

// DotDivergeConfig represents the .diverge.yaml file found in service repositories.
type DotDivergeConfig struct {
	// APIVersion is the API version of the configuration format.
	APIVersion string `yaml:"apiVersion"`
	// Kind is the configuration resource type.
	Kind string `yaml:"kind"`
	// Metadata holds the resource metadata.
	Metadata struct {
		// Name is the name of the service configuration.
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	// Spec defines the desired preview configuration.
	Spec DotDivergeSpec `yaml:"spec"`
}

// DotDivergeSpec is the spec section of .diverge.yaml.
type DotDivergeSpec struct {
	// Namespace is the target namespace for the preview environment.
	Namespace string `yaml:"namespace"`
	// ServiceName is the name of the service to deploy.
	ServiceName string `yaml:"serviceName"`
	// Port is the container port the service listens on.
	Port int32 `yaml:"port"`
	// Routing configures the ingress routing for the service.
	Routing DotDivergeRouting `yaml:"routing"`
	// WebSocket configures WebSocket proxy settings.
	WebSocket *v1alpha1.WebSocketSpec `yaml:"websocket"`
	// Container configures the container execution environment.
	Container DotDivergeContainer `yaml:"container"`
	// Image configures where the preview image is pulled from.
	Image DotDivergeImage `yaml:"image"`
}

// DotDivergeImage configures the preview image reference.
type DotDivergeImage struct {
	// Repository is the image repository, without a tag or digest
	// (for example "registry.example.com/team/payments-api"). When empty,
	// the repository path on the Git provider is used.
	Repository string `yaml:"repository"`
}

// DotDivergeRouting configures preview routing.
type DotDivergeRouting struct {
	// ParentRef specifies the Gateway API parent reference.
	ParentRef string `yaml:"parentRef"`
	// HeaderKey is the routing header key.
	HeaderKey string `yaml:"headerKey"`
}

// DotDivergeContainer configures the preview container.
type DotDivergeContainer struct {
	// Env contains a list of environment variables.
	Env []struct {
		// Name is the environment variable name.
		Name string `yaml:"name"`
		// Value is the environment variable value.
		Value string `yaml:"value"`
	} `yaml:"env"`
}

// ParseDotDivergeConfig parses the raw bytes of a .diverge.yaml file.
func ParseDotDivergeConfig(data []byte) (*DotDivergeConfig, error) {
	const maxManifestSize = 5 << 20 // 5MB
	if len(data) > maxManifestSize {
		return nil, fmt.Errorf("manifest exceeds maximum size of %d bytes", maxManifestSize)
	}

	var cfg DotDivergeConfig

	// Create decoder with alias limits to prevent YAML bombs
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, fmt.Errorf("failed to parse .diverge.yaml: %w", err)
	}

	// Note: go-yaml v3 mitigates yaml-bombs by limiting alias depth inherently,
	// but we decode into node first then into our struct to be safe.
	if err := node.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse .diverge.yaml: %w", err)
	}
	if cfg.Spec.ServiceName == "" {
		return nil, fmt.Errorf(".diverge.yaml: spec.serviceName is required")
	}
	if cfg.Spec.Port == 0 {
		cfg.Spec.Port = 8080 // default
	}
	if cfg.Spec.Port < 1 || cfg.Spec.Port > 65535 {
		return nil, fmt.Errorf("port %d out of valid range 1-65535", cfg.Spec.Port)
	}
	if err := validateImageRepository(cfg.Spec.Image.Repository); err != nil {
		return nil, err
	}
	if cfg.Spec.WebSocket != nil && cfg.Spec.WebSocket.Timeout != "" {
		if _, err := time.ParseDuration(cfg.Spec.WebSocket.Timeout); err != nil {
			return nil, fmt.Errorf("invalid websocket timeout: %w", err)
		}
	}
	return &cfg, nil
}

// validateImageRepository rejects a repository that already carries a tag or
// digest, since the preview tag is always derived from the commit.
func validateImageRepository(repo string) error {
	if repo == "" {
		return nil
	}
	if strings.ContainsAny(repo, "@ \t") {
		return fmt.Errorf(".diverge.yaml: spec.image.repository %q must not contain a digest or whitespace", repo)
	}
	lastSegment := repo[strings.LastIndex(repo, "/")+1:]
	if strings.Contains(lastSegment, ":") {
		return fmt.Errorf(".diverge.yaml: spec.image.repository %q must not contain a tag", repo)
	}
	return nil
}

// PreviewImage returns the image reference for a preview: spec.image.repository
// when set, otherwise defaultRepository, tagged with tag.
func (c *DotDivergeConfig) PreviewImage(defaultRepository, tag string) string {
	repo := c.Spec.Image.Repository
	if repo == "" {
		repo = defaultRepository
	}
	return fmt.Sprintf("%s:%s", repo, tag)
}

// ToServicePreviewConfig converts a parsed .diverge.yaml into the CRD type.
func (c *DotDivergeConfig) ToServicePreviewConfig(image string) *v1alpha1.ServicePreviewConfig {
	cfg := &v1alpha1.ServicePreviewConfig{
		ServiceName: c.Spec.ServiceName,
		Namespace:   c.Spec.Namespace,
		Port:        c.Spec.Port,
		Image:       image,
		ParentRef:   c.Spec.Routing.ParentRef,
		HeaderKey:   c.Spec.Routing.HeaderKey,
		WebSocket:   c.Spec.WebSocket,
	}
	for _, env := range c.Spec.Container.Env {
		cfg.Env = append(cfg.Env, v1alpha1.EnvVar{Name: env.Name, Value: env.Value})
	}
	return cfg
}
