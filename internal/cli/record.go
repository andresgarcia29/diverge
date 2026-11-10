package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/divergedev/diverge/pkg/replay"
)

type recordOptions struct {
	targetURL         string
	listenAddr        string
	outputFile        string
	stripAuth         bool
	stripHeaders      []string
	redactQueryParams []string
	redactBodyFields  []string
	autoChain         bool
	maxRequests       int
	duration          time.Duration
}

// newRecordCmd constructs the `diverge record` subcommand for capturing and sanitizing live HTTP, ConnectRPC, and Protobuf/gRPC traffic.
func newRecordCmd(app *App) *cobra.Command {
	opts := &recordOptions{}

	cmd := &cobra.Command{
		Use:   "record [env-name] --target <upstream-url> --output <trace.jsonl>",
		Short: "Capture and sanitize live HTTP, ConnectRPC, and gRPC traffic into a replayable JSONL trace",
		Long: `Starts a local reverse-proxy recorder that forwards incoming HTTP, ConnectRPC, and
Protobuf/gRPC requests to an upstream target URL (or a Diverge preview environment's gateway URL),
scrubs credentials and sensitive fields in flight, and writes a deterministic JSONL trace ready
for 'diverge replay'.`,
		Example: `	# Record 20 requests against staging and scrub auth + sensitive JSON body fields
	diverge record --target https://staging.example.com --output ./traces/checkout.jsonl --max-requests 20 --redact-body-field password,cvv

	# Record traffic for environment pr-42 for 30 seconds
	diverge record pr-42 --output ./traces/pr-42.jsonl --duration 30s`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			target := strings.TrimSpace(opts.targetURL)
			var headerKey, headerVal string
			if target == "" && len(args) == 1 {
				envName := args[0]
				cOpts := &curlOptions{}
				resolvedURL, hk, hv, err := resolveCurlRouting(cmd, app, envName, "/", cOpts)
				if err != nil {
					return fmt.Errorf("resolve target URL for environment %s: %w", envName, err)
				}
				target = resolvedURL
				headerKey = hk
				headerVal = hv
			}
			if target == "" {
				return fmt.Errorf("--target <upstream-url> (or an <env-name> argument) is required")
			}

			outPath := strings.TrimSpace(opts.outputFile)
			if outPath == "" {
				outPath = "trace.jsonl"
			}

			var traceWriter io.Writer
			var outFile *os.File
			if outPath == "-" {
				traceWriter = cmd.OutOrStdout()
			} else {
				f, err := os.Create(outPath)
				if err != nil {
					return fmt.Errorf("create output trace file %s: %w", outPath, err)
				}
				outFile = f
				defer func() { _ = outFile.Close() }()
				traceWriter = outFile
			}

			proxy, err := replay.NewRecorderProxy(target, traceWriter, replay.SanitizeOptions{
				StripAuth:         opts.stripAuth,
				StripHeaders:      opts.stripHeaders,
				RedactQueryParams: opts.redactQueryParams,
				RedactBodyFields:  opts.redactBodyFields,
			})
			if err != nil {
				return err
			}
			proxy.HeaderKey = headerKey
			proxy.HeaderValue = headerVal
			proxy.AutoChain = opts.autoChain
			proxy.MaxRequests = opts.maxRequests

			runCtx := cmd.Context()
			var cancel context.CancelFunc
			if opts.duration > 0 {
				runCtx, cancel = context.WithTimeout(runCtx, opts.duration)
			} else {
				runCtx, cancel = context.WithCancel(runCtx)
			}
			defer cancel()

			proxy.OnRecorded = func(entry replay.RequestEntry) {
				if outPath != "-" {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[%s] %s %s -> HTTP %d\n", entry.ID, entry.Method, entry.Path, entry.ExpectStatus)
				}
				if opts.maxRequests > 0 && proxy.Count() >= opts.maxRequests {
					cancel()
				}
			}

			listenAddr := strings.TrimSpace(opts.listenAddr)
			if listenAddr == "" {
				listenAddr = "127.0.0.1:8095"
			}
			ln, err := net.Listen("tcp", listenAddr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", listenAddr, err)
			}

			srv := replay.NewRecorderServer(proxy)

			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Recording proxy listening on http://%s -> %s (output: %s)\n", ln.Addr().String(), target, outPath)

			errCh := make(chan error, 1)
			go func() {
				if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
					errCh <- serveErr
				}
				close(errCh)
			}()

			select {
			case <-runCtx.Done():
			case serveErr := <-errCh:
				if serveErr != nil {
					return serveErr
				}
			}

			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutCancel()
			_ = srv.Shutdown(shutCtx)

			if outPath != "-" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Recorded %d sanitized request(s) to %s\n", proxy.Count(), outPath)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&opts.targetURL, "target", "t", "", "Upstream HTTP/gRPC target URL to proxy and record")
	cmd.Flags().StringVarP(&opts.listenAddr, "listen", "l", "127.0.0.1:8095", "Local address for the recording reverse proxy")
	cmd.Flags().StringVarP(&opts.outputFile, "output", "o", "trace.jsonl", "Output JSONL trace file path (or '-' for stdout)")
	cmd.Flags().BoolVar(&opts.stripAuth, "strip-auth", true, "Strip Authorization, Cookie, X-Api-Key headers, userinfo, and auth query tokens")
	cmd.Flags().StringSliceVar(&opts.stripHeaders, "strip-header", nil, "Additional request header name to strip before recording (repeatable)")
	cmd.Flags().StringSliceVar(&opts.redactQueryParams, "redact-query", nil, "Additional query parameter name to redact with [REDACTED] (repeatable)")
	cmd.Flags().StringSliceVar(&opts.redactBodyFields, "redact-body-field", nil, "JSON request body field name or dot-path to redact with [REDACTED] (repeatable)")
	cmd.Flags().BoolVar(&opts.autoChain, "auto-chain", false, "Automatically extract created resource IDs from POST/PUT responses and parameterize subsequent request paths")
	cmd.Flags().IntVar(&opts.maxRequests, "max-requests", 0, "Stop recording automatically after capturing N requests (0 = unlimited)")
	cmd.Flags().DurationVarP(&opts.duration, "duration", "d", 0, "Stop recording automatically after this duration (e.g. 30s)")

	return cmd
}
