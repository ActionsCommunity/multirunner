import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: true,
  retries: 0,
  reporter: 'line',
  use: {
    baseURL: 'http://127.0.0.1:4173',
    trace: 'retain-on-failure',
  },
  webServer: {
    command: 'pnpm dev:demo --host 127.0.0.1 --port 4173',
    url: 'http://127.0.0.1:4173',
    reuseExistingServer: false,
    timeout: 120_000,
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
      grepInvert: /\[matrix:/,
    },
    {
      name: 'mobile',
      use: { ...devices['Pixel 7'] },
      grep: /\[matrix:mobile\]/,
    },
    {
      name: 'high-zoom',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 640, height: 450 },
      },
      grep: /\[matrix:high-zoom\]/,
    },
    {
      name: 'keyboard',
      use: { ...devices['Desktop Chrome'] },
      grep: /\[matrix:keyboard\]/,
    },
    {
      name: 'reduced-motion',
      use: { ...devices['Desktop Chrome'], reducedMotion: 'reduce' },
      grep: /\[matrix:reduced-motion\]/,
    },
    {
      name: 'offline',
      use: { ...devices['Desktop Chrome'] },
      grep: /\[matrix:offline\]/,
    },
  ],
})
