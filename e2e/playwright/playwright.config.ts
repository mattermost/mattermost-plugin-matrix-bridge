import {defineConfig, devices} from '@playwright/test';

export default defineConfig({
    testDir: 'tests',

    // Tests share the two launcher environments and may mutate them.
    fullyParallel: false,
    workers: 1,
    retries: 0,
    forbidOnly: Boolean(process.env.CI),
    timeout: 60_000,
    expect: {timeout: 15_000},
    reporter: process.env.CI ? [['github'], ['html', {open: 'never'}]] : 'list',
    globalSetup: './global-setup.ts',
    globalTeardown: './global-teardown.ts',
    use: {
        trace: 'retain-on-failure',
        screenshot: 'only-on-failure',
    },
    projects: [{name: 'chromium', use: {...devices['Desktop Chrome']}}],
});
