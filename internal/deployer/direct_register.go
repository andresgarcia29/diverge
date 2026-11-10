package deployer

import (
	"flag"
	"fmt"
	"os"

	"github.com/divergedev/diverge/pkg/registry"
)

var (
	manifestSourceType = flag.String("manifest-source-type", "configmap", "Manifest source type for direct deployer (configmap|url|serviceconfig)")
)

// RequiresServiceConfig reports whether the given deploy provider fails on an
// Environment without a ServiceConfig, so a project needs a .diverge.yaml.
func RequiresServiceConfig(provider string) bool {
	return provider == "direct" && *manifestSourceType == "serviceconfig"
}

func init() {
	Providers.Register("direct", registry.Provider[Deployer]{
		Create: func(deps registry.Deps) (Deployer, error) {
			var fetcher ManifestFetcher
			switch *manifestSourceType {
			case "url":
				manifestToken := os.Getenv("DIVERGE_MANIFEST_TOKEN")
				fetcher = NewURLFetcher(manifestToken)
			case "serviceconfig":
				fetcher = &ServiceConfigFetcher{}
			case "configmap", "":
				fetcher = &ConfigMapFetcher{Client: deps.Client}
			default:
				return nil, fmt.Errorf("unsupported manifest source type: %q", *manifestSourceType)
			}
			return &DirectDeployer{
				Client:  deps.Client,
				Fetcher: fetcher,
			}, nil
		},
		Description: "Direct deployment (configmap, url, or serviceconfig)",
	})
}
