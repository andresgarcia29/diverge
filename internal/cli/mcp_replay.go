package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/protocgen/proto2mcp/pkg/mcpruntime"

	divergev1alpha1 "github.com/divergedev/diverge/api/gen/diverge/v1alpha1"
	divergev1alpha1connect "github.com/divergedev/diverge/api/gen/diverge/v1alpha1/divergev1alpha1connect"
	"github.com/divergedev/diverge/pkg/replay"
)

var replaySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {"type": "string", "description": "Name of the preview environment to replay traffic against"},
		"namespace": {"type": "string", "description": "Kubernetes namespace of the environment (default: default)"},
		"base_url": {"type": "string", "description": "Baseline gateway URL (auto-discovered from environment if omitted)"},
		"preview_url": {"type": "string", "description": "Optional separate preview URL when subdomain routing is used"},
		"routing_key": {"type": "string", "description": "Override routing header value (defaults to environment name)"},
		"trace_file": {"type": "string", "description": "Path to a .jsonl, .har, or .json trace file to replay"},
		"requests": {
			"type": "array",
			"description": "Inline list of HTTP/ConnectRPC/Protobuf requests to replay if trace_file is not provided",
			"items": {
				"type": "object",
				"properties": {
					"id": {"type": "string"},
					"method": {"type": "string"},
					"path": {"type": "string"},
					"headers": {"type": "object"},
					"body": {"type": "string"},
					"body_base64": {"type": "string"},
					"proto_message": {"type": "string"},
					"expect_status": {"type": "integer"}
				},
				"required": ["path"]
			}
		},
		"compare_values": {"type": "boolean", "description": "Compare exact scalar values in addition to JSON/Protobuf field types and wire schema"},
		"fail_on_extra_fields": {"type": "boolean", "description": "Treat newly added JSON keys or Protobuf tags in preview as breaking"},
		"ignore_fields": {"type": "array", "items": {"type": "string"}, "description": "Field names or paths to ignore during contract diffing"},
		"filter_hosts": {"type": "array", "items": {"type": "string"}, "description": "Optional hostnames or wildcard patterns (e.g. *.example.com) to retain from multi-host traces"},
		"strip_headers": {"type": "array", "items": {"type": "string"}, "description": "Optional request header names to strip before replaying"},
		"strip_auth": {"type": "boolean", "description": "Strip Authorization, Proxy-Authorization, Cookie, and X-Api-Key headers before replaying"},
		"proto_descriptor": {"type": "string", "description": "Optional path to a compiled Protobuf FileDescriptorSet (.binpb)"},
		"proto_message": {"type": "string", "description": "Optional default fully-qualified Protobuf message type for response decoding"},
		"max_latency_ms": {"type": "integer", "description": "Maximum acceptable preview response latency per request in milliseconds"}
	}
}`)

type mcpReplayParams struct {
	Name              string          `json:"name"`
	Namespace         string          `json:"namespace"`
	BaseURL           string          `json:"base_url"`
	PreviewURL        string          `json:"preview_url"`
	RoutingKey        string          `json:"routing_key"`
	TraceFile         string          `json:"trace_file"`
	Requests          json.RawMessage `json:"requests"`
	CompareValues     bool            `json:"compare_values"`
	FailOnExtraFields bool            `json:"fail_on_extra_fields"`
	IgnoreFields      []string        `json:"ignore_fields"`
	FilterHosts       []string        `json:"filter_hosts"`
	StripHeaders      []string        `json:"strip_headers"`
	StripAuth         bool            `json:"strip_auth"`
	ProtoDescriptor   string          `json:"proto_descriptor"`
	ProtoMessage      string          `json:"proto_message"`
	MaxLatencyMs      int64           `json:"max_latency_ms"`
}

// registerReplay registers the diverge_replay MCP tool handler.
func registerReplay(registry mcpruntime.Registry, client divergev1alpha1connect.EnvironmentServiceClient) {
	registry.Register(mcpruntime.ToolDefinition{
		Name:        "diverge_replay",
		Description: "Replay recorded HTTP, ConnectRPC, and Protobuf/gRPC traffic (.jsonl, .har, .json, or inline requests) against baseline and a preview environment. Compares status code parity, p50/p95 latency, JSON structural schemas, and binary Protobuf wire tags/descriptors. Executes requests against baseline and attempts preview only when baseline execution completes without error (including passing the 10 MiB response-size check, whereas baseline HTTP or gRPC status does not determine whether preview is attempted). Automatically strips recorded diverge_env sticky-routing cookies, diverge.env baggage items, and routing headers from baseline requests; supports filter_hosts to retain only matching hostnames, strip_auth to remove Authorization/Proxy-Authorization/Cookie/X-Api-Key headers and URL userinfo, and strip_headers to remove custom headers before replay.",
		InputSchema: replaySchema,
	}, func(ctx context.Context, req mcpruntime.ToolRequest) (*mcpruntime.CallToolResult, error) {
		var params mcpReplayParams
		if err := json.Unmarshal(req.Arguments, &params); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}

		var (
			entries []replay.RequestEntry
			err     error
		)
		if strings.TrimSpace(params.TraceFile) != "" {
			entries, err = replay.LoadTrace(params.TraceFile)
			if err != nil {
				return errorToolResult(err), nil
			}
		} else if len(params.Requests) > 0 && string(params.Requests) != "null" {
			entries, err = replay.ParseTrace(params.Requests, ".json")
			if err != nil {
				return errorToolResult(err), nil
			}
		} else {
			return errorToolResult(fmt.Errorf("either trace_file or requests is required")), nil
		}

		if len(params.FilterHosts) > 0 {
			entries = replay.FilterEntriesByHost(entries, params.FilterHosts)
			if len(entries) == 0 {
				return errorToolResult(fmt.Errorf("no trace entries matched filter_hosts %v", params.FilterHosts)), nil
			}
		}

		protoFiles, err := replay.LoadDescriptorFiles(params.ProtoDescriptor)
		if err != nil {
			return errorToolResult(err), nil
		}

		envName := strings.TrimSpace(params.Name)
		ns := strings.TrimSpace(params.Namespace)
		if ns == "" {
			ns = "default"
		}
		baseURL := strings.TrimSpace(params.BaseURL)
		previewURL := strings.TrimSpace(params.PreviewURL)
		headerKey := replay.DefaultHeaderKey
		headerValue := strings.TrimSpace(params.RoutingKey)
		if headerValue == "" {
			headerValue = envName
		}

		allAbsolute := allEntriesAbsolute(entries)
		canSkipDiscovery := (previewURL != "" || strings.TrimSpace(params.RoutingKey) != "") && (baseURL != "" || allAbsolute)

		if envName != "" && client != nil {
			resp, getErr := client.GetEnvironment(ctx, connect.NewRequest(&divergev1alpha1.GetEnvironmentRequest{
				Name:      envName,
				Namespace: ns,
			}))
			if getErr != nil {
				if !canSkipDiscovery {
					return errorToolResult(getErr), nil
				}
			} else if env := resp.Msg.GetEnvironment(); env != nil {
				routing := env.GetSpec().GetRouting()
				if hk := strings.TrimSpace(routing.GetHeaderKey()); hk != "" {
					headerKey = hk
				}
				if strings.TrimSpace(params.RoutingKey) == "" {
					if hv := strings.TrimSpace(routing.GetHeaderValue()); hv != "" {
						headerValue = hv
					}
				}

				statusURL := strings.TrimSpace(env.GetStatus().GetUrl())
				externalURL := strings.TrimSpace(routing.GetExternalUrl())
				subdomainRouted := strings.EqualFold(strings.TrimSpace(routing.GetMode()), "subdomain")

				if subdomainRouted {
					if previewURL == "" {
						previewURL = statusURL
					}
					if previewURL == "" {
						return errorToolResult(fmt.Errorf("environment %s uses subdomain routing but has no published preview URL; provide preview_url", envName)), nil
					}
					if baseURL == "" {
						baseURL = externalURL
					}
					if baseURL == "" && !allAbsolute {
						return errorToolResult(fmt.Errorf("environment %s uses subdomain routing (%s); base_url is required to compare against baseline", envName, previewURL)), nil
					}
				} else if baseURL == "" {
					baseURL = statusURL
					if baseURL == "" {
						baseURL = externalURL
					}
				}
			}
		}

		if previewURL == "" && headerValue == "" {
			return errorToolResult(fmt.Errorf("one of name, routing_key, or preview_url is required to target a preview environment")), nil
		}
		if previewURL != "" && previewURL == baseURL && headerValue == "" {
			return errorToolResult(fmt.Errorf("preview_url must differ from base_url when name and routing_key are omitted")), nil
		}

		var maxLatency time.Duration
		if params.MaxLatencyMs > 0 {
			maxLatency = time.Duration(params.MaxLatencyMs) * time.Millisecond
		}

		runner := replay.NewRunner(replay.Config{
			Environment:        envName,
			Namespace:          ns,
			BaseURL:            baseURL,
			PreviewBaseURL:     previewURL,
			HeaderKey:          headerKey,
			HeaderValue:        headerValue,
			MaxLatency:         maxLatency,
			FailOnStatusDiff:   true,
			FailOnContractDiff: true,
			FilterHosts:        params.FilterHosts,
			StripHeaders:       params.StripHeaders,
			StripAuth:          params.StripAuth,
			DiffOptions: replay.DiffOptions{
				CompareValues:     params.CompareValues,
				FailOnExtraFields: params.FailOnExtraFields,
				IgnoreFields:      params.IgnoreFields,
				ProtoFiles:        protoFiles,
				ProtoMessage:      params.ProtoMessage,
			},
		})

		report, err := runner.Run(ctx, entries)
		if err != nil {
			return errorToolResult(err), nil
		}

		data, err := json.Marshal(report)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal replay report: %w", err)
		}

		return &mcpruntime.CallToolResult{
			Content: json.RawMessage(data),
			IsError: !report.Passed,
		}, nil
	})
}
