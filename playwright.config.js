const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  workers: process.env.CI ? 2 : 1,
  timeout: 60_000,
  forbidOnly: Boolean(process.env.CI),
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: 'http://127.0.0.1:4173',
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'chromium-desktop', use: { ...devices['Desktop Chrome'] } },
    { name: 'webkit-desktop', use: { ...devices['Desktop Safari'] } },
    { name: 'firefox-desktop', use: { ...devices['Desktop Firefox'] } },
    {
      name: 'chromium-mobile',
      use: { ...devices['Pixel 7'] },
    },
    {
      name: 'webkit-mobile',
      use: { ...devices['iPhone 13'] },
    },
  ],
  webServer: [{
    command: 'node e2e/support/static-server.cjs',
    url: 'http://127.0.0.1:4173/camera.html',
    reuseExistingServer: false,
  }, {
    command: 'go test ./internal/api -run ^TestBrowserAcceptanceServer$ -count=1 -timeout=30m',
    env: { VEDETTA_BROWSER_ACCEPTANCE: '1' },
    url: 'http://127.0.0.1:4175/login.html',
    reuseExistingServer: false,
    timeout: 120_000,
  }],
});
