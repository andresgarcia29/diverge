import { render, screen, waitFor, fireEvent } from '@/test/utils'
import { server } from '@/test/mocks/server'
import { http, HttpResponse } from 'msw'
import { describe, it, expect, afterEach, vi } from 'vitest'
import { VerifyReplayTab } from './VerifyReplayTab'

describe('VerifyReplayTab', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('runs environment verification and renders check outcomes and auto-fix link', async () => {
    let capturedBody: Record<string, unknown> | null = null
    server.use(
      http.post('*/api/v1/environments/verify', async ({ request }) => {
        capturedBody = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({
          environment: 'pr-42',
          namespace: 'default',
          routingHeader: 'x-diverge-env: pr-42',
          passed: false,
          autoFixTaskName: 'autofix-pr-42-verify',
          checks: [
            {
              name: 'crd.phase',
              passed: true,
              status: 'Ready',
              message: 'Environment phase is Ready',
            },
            {
              name: 'probe.preview./healthz',
              passed: false,
              status: 'HTTP 502',
              durationMs: 18,
              message: 'GET https://app.example.com/healthz returned 502',
            },
          ],
        })
      }),
    )

    render(<VerifyReplayTab namespace="default" environmentName="pr-42" />)

    fireEvent.click(screen.getByLabelText(/Auto-Fix on Failure/i))
    fireEvent.click(screen.getByRole('button', { name: /Run Verification/i }))

    await waitFor(() => {
      expect(screen.getByTestId('verify-result-panel')).toBeInTheDocument()
    })

    expect(capturedBody).toMatchObject({
      name: 'pr-42',
      namespace: 'default',
      paths: ['/healthz', '/readyz'],
      compareBaseline: true,
      autoFix: true,
    })

    expect(screen.getByText(/Verification Failed \(1\/2 checks passed\)/i)).toBeInTheDocument()
    expect(screen.getByText('crd.phase')).toBeInTheDocument()
    expect(screen.getByText('probe.preview./healthz')).toBeInTheDocument()
    expect(screen.getByText('autofix-pr-42-verify')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /View AgentTask/i })).toHaveAttribute(
      'href',
      '/tasks/default/autofix-pr-42-verify',
    )
  })

  it('runs contract replay with sanitization options and displays breaking contract diffs', async () => {
    let capturedBody: Record<string, unknown> | null = null
    server.use(
      http.post('*/api/v1/environments/replay', async ({ request }) => {
        capturedBody = (await request.json()) as Record<string, unknown>
        return HttpResponse.json({
          environment: 'pr-42',
          namespace: 'default',
          totalRequests: 1,
          passedRequests: 0,
          failedRequests: 1,
          statusMismatches: 0,
          breakingDiffs: 1,
          baselineP50Ms: 12,
          baselineP95Ms: 12,
          previewP50Ms: 19,
          previewP95Ms: 19,
          passed: false,
          autoFixTaskName: 'autofix-pr-42-replay',
          results: [
            {
              index: 0,
              method: 'POST',
              path: '/api/orders',
              protocol: 'json',
              baselineStatus: 200,
              previewStatus: 200,
              baselineLatencyMs: 12,
              previewLatencyMs: 19,
              statusMatch: true,
              breakingDiffCount: 1,
              passed: false,
              extractedVars: {
                order_id: 'ord-prev-99',
              },
              contractDiffs: [
                {
                  path: '$.totalCents',
                  kind: 'missing_field',
                  baseline: 'number',
                  preview: 'missing',
                  breaking: true,
                },
              ],
            },
          ],
        })
      }),
    )

    render(<VerifyReplayTab namespace="default" environmentName="pr-42" />)

    fireEvent.change(screen.getByLabelText(/Trace Payload/i), {
      target: { value: '{"method":"POST","path":"/api/orders","body":"{\\"secret\\":\\"s3cr3t\\"}"}' },
    })
    fireEvent.change(screen.getByLabelText(/Redact Query Params/i), {
      target: { value: 'custom_token' },
    })
    fireEvent.change(screen.getByLabelText(/Redact Request Body Fields/i), {
      target: { value: '$.secret' },
    })
    fireEvent.click(screen.getByLabelText(/Auto-Fix on Contract Drift/i))
    fireEvent.click(screen.getByRole('button', { name: /Run Contract Replay/i }))

    await waitFor(() => {
      expect(screen.getByTestId('replay-result-panel')).toBeInTheDocument()
    })

    expect(capturedBody).toMatchObject({
      name: 'pr-42',
      namespace: 'default',
      stripAuth: true,
      redactQueryParams: ['custom_token'],
      redactBodyFields: ['$.secret'],
      autoFix: true,
    })

    expect(screen.getByText(/Contract Replay Failed/i)).toBeInTheDocument()
    expect(screen.getByText('POST /api/orders')).toBeInTheDocument()
    expect(screen.getByText('order_id=ord-prev-99')).toBeInTheDocument()
    expect(screen.getByText('$.totalCents')).toBeInTheDocument()
    expect(screen.getByText('autofix-pr-42-replay')).toBeInTheDocument()
  })
})
