import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  workers: 1,
  reporter: 'line',
  use: {
    baseURL: 'http://127.0.0.1:4173/admin',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } },
  ],
  webServer: [
    {
      command: 'node tests/mock-server.mjs',
      url: 'http://127.0.0.1:4174/healthz',
      reuseExistingServer: false,
    },
    {
      command: 'VITE_API_TARGET=http://127.0.0.1:4174 npm run dev -- --port 4173',
      url: 'http://127.0.0.1:4173/admin/login',
      reuseExistingServer: false,
    },
  ],
})
