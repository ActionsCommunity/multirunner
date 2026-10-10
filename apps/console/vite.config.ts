import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { configDefaults, defineConfig, type Plugin } from 'vitest/config'
import type { ProxyOptions } from 'vite'

export const consoleWorkbox = {
  navigateFallback: '/index.html',
  navigateFallbackDenylist: [/^\/api\//],
  runtimeCaching: [
    {
      urlPattern: /^https?:\/\/[^/]+\/api\//,
      handler: 'NetworkOnly' as const,
      method: 'GET' as const,
    },
  ],
}

export function resolveLocalConsoleTarget(value?: string) {
  const target = new URL(value ?? 'http://127.0.0.1:9092')
  if (
    target.protocol !== 'http:' ||
    !['127.0.0.1', 'localhost', '[::1]'].includes(target.hostname)
  ) {
    throw new Error('local console proxy target must use HTTP on loopback')
  }
  return target.origin
}

export function localConsoleProxy(target: string): ProxyOptions {
  const targetURL = new URL(target)
  return {
    target,
    changeOrigin: true,
    configure(proxy) {
      proxy.on('proxyReq', (request) => {
        request.setHeader('Host', targetURL.host)
        request.setHeader('Origin', target)
      })
    },
  }
}

function sendJSON(response: ServerResponse, body: unknown) {
  response.statusCode = 200
  response.setHeader('Content-Type', 'application/json')
  response.end(JSON.stringify(body))
}

function demoConsoleAPI(): Plugin {
  return {
    name: 'multirunner-demo-console-api',
    configureServer(server) {
      server.middlewares.use(
        '/api',
        (request: IncomingMessage, response: ServerResponse, next) => {
          const path = request.url?.split('?')[0] ?? ''
          if (path === '/summary') {
            sendJSON(response, {
              runner_sessions: 0,
              pending_sessions: 0,
              workflow_runs: 0,
              workflow_jobs: 0,
              successful_jobs: 0,
              failed_jobs: 0,
              cancelled_jobs: 0,
              average_duration_seconds: 0,
            })
            return
          }
          if (path === '/runs') {
            sendJSON(response, { runs: [] })
            return
          }
          if (path === '/v1/session') {
            sendJSON(response, {
              actor_id: 'demo-operator',
              csrf_token: 'demo-csrf',
            })
            return
          }
          if (path === '/v1/saved-views') {
            sendJSON(response, { items: [], count: 0 })
            return
          }
          if (path === '/v1/runners') {
            sendJSON(response, {
              items: [],
              applied_filters: {},
              count: 0,
              host_epoch: 'demo-epoch',
              max_sequence: 0,
            })
            return
          }
          if (path === '/v1/events') {
            response.statusCode = 200
            response.setHeader('Content-Type', 'text/event-stream')
            response.setHeader('Cache-Control', 'no-cache')
            response.setHeader('Connection', 'keep-alive')
            response.flushHeaders()
            response.write(': demo stream ready\n\n')
            response.write(
              'event: replay-complete\ndata: {"host_epoch":"demo-epoch","sequence":0,"replayed":0}\n\n',
            )
            return
          }
          next()
        },
      )
    },
  }
}

export default defineConfig(({ command, mode }) => {
  const localTarget =
    command === 'serve' && ['machine', 'machine-legacy'].includes(mode)
      ? resolveLocalConsoleTarget(process.env.MULTIRUNNER_CONSOLE_URL)
      : undefined

  return {
    build: {
      emptyOutDir: true,
      outDir: '../../internal/consoleui/dist',
    },
    plugins: [
      react(),
      ...(command === 'serve' && mode === 'demo' ? [demoConsoleAPI()] : []),
      VitePWA({
      registerType: 'prompt',
      includeAssets: ['icon.svg'],
      manifest: {
        name: 'Multirunner Operations Console',
        short_name: 'Multirunner',
        description: 'Local operations console for Multirunner hosts.',
        theme_color: '#101c2a',
        background_color: '#f3f7fa',
        display: 'standalone',
        start_url: '/',
        icons: [
          {
            src: '/icon.svg',
            sizes: 'any',
            type: 'image/svg+xml',
            purpose: 'any maskable',
          },
        ],
      },
      workbox: {
        ...consoleWorkbox,
      },
      }),
    ],
    server: localTarget
      ? {
          proxy: {
            '/api': localConsoleProxy(localTarget),
            '/auth': localConsoleProxy(localTarget),
          },
        }
      : undefined,
    test: {
      coverage: {
        provider: 'v8',
        reporter: ['text', 'json-summary'],
        reportsDirectory: './coverage',
        thresholds: {
          statements: 74,
          branches: 60,
          functions: 70,
          lines: 79,
        },
      },
      environment: 'jsdom',
      exclude: [...configDefaults.exclude, 'e2e/**'],
      setupFiles: './src/test/setup.ts',
    },
  }
})
