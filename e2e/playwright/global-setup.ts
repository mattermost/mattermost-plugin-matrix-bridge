import {type ChildProcess, execFileSync, spawn} from 'node:child_process';
import {existsSync, mkdirSync, rmSync, writeFileSync} from 'node:fs';
import path from 'node:path';
import {createInterface} from 'node:readline';
import {setTimeout as sleep} from 'node:timers/promises';

import {request} from '@playwright/test';

import {stopLaunchers} from './global-teardown';
import {authFile, envFile, envNames, pidsFile, readEnv, stateDir, type EnvName} from './support/env';

const repoRoot = path.resolve(__dirname, '..', '..');
const launcher = path.join(stateDir, 'e2e-env');
const startupTimeoutMs = 15 * 60 * 1000;

export default async function globalSetup(): Promise<void> {
    const bundle = process.env.E2E_PLUGIN_BUNDLE;
    if (!bundle || !path.isAbsolute(bundle) || !existsSync(bundle)) {
        throw new Error('E2E_PLUGIN_BUNDLE must be the absolute path of a built plugin bundle; run the UI tests with `make e2e-ui`');
    }

    rmSync(stateDir, {recursive: true, force: true});
    mkdirSync(stateDir, {recursive: true});

    // A built binary, not `go run`, which doesn't forward SIGTERM to the launcher.
    execFileSync('go', ['build', '-o', launcher, './e2e/cmd/e2e-env'], {cwd: repoRoot, stdio: 'inherit'});

    const children = envNames.map((name) => startLauncher(name, bundle));
    writeFileSync(pidsFile, JSON.stringify(children.map((child) => child.pid)));
    try {
        await Promise.all(envNames.map((name, i) => waitForEnv(name, children[i])));
        await Promise.all(envNames.map(storeAdminAuth));
    } catch (err) {
        await stopLaunchers(children.map((child) => child.pid!));
        throw err;
    }
}

function startLauncher(name: EnvName, bundle: string): ChildProcess {
    const args = ['--out', envFile(name)];
    if (name === 'noServer') {
        args.push('--no-server');
    }
    const child = spawn(launcher, args, {
        cwd: repoRoot,
        env: {...process.env, E2E_PLUGIN_BUNDLE: bundle},
        stdio: ['ignore', 'ignore', 'pipe'],
    });
    const log = (line: string) => process.stderr.write(`[e2e-env ${name}] ${line}\n`);
    child.on('error', (err) => log(err.message));
    createInterface({input: child.stderr!}).on('line', log);
    return child;
}

async function waitForEnv(name: EnvName, child: ChildProcess): Promise<void> {
    const deadline = Date.now() + startupTimeoutMs;
    while (!existsSync(envFile(name))) {
        if (child.pid === undefined || child.exitCode !== null || child.signalCode !== null) {
            throw new Error(`e2e-env ${name} exited before it was ready (code ${child.exitCode}, signal ${child.signalCode})`);
        }
        if (Date.now() > deadline) {
            throw new Error(`e2e-env ${name} not ready after ${startupTimeoutMs / 60000} minutes`);
        }
        await sleep(1000);
    }
}

// storeAdminAuth logs the admin in through the API and saves the session cookies, so tests start
// signed in without driving the login form. It also dismisses what Mattermost overlays on a first
// admin's first visit: the onboarding task list (a user preference) and the desktop-app landing
// page (a localStorage flag).
async function storeAdminAuth(name: EnvName): Promise<void> {
    const env = readEnv(name);
    const context = await request.newContext({baseURL: env.mattermost_url});
    try {
        const resp = await context.post('/api/v4/users/login', {
            headers: {'X-Requested-With': 'XMLHttpRequest'},
            data: {login_id: env.admin_username, password: env.admin_password},
        });
        if (!resp.ok()) {
            throw new Error(`admin login to the ${name} env failed with status ${resp.status()}`);
        }

        const state = await context.storageState();
        const csrf = state.cookies.find((cookie) => cookie.name === 'MMCSRF')?.value ?? '';
        const userId = (await resp.json()).id as string;
        const prefs = await context.put(`/api/v4/users/${userId}/preferences`, {
            headers: {'X-Requested-With': 'XMLHttpRequest', 'X-CSRF-Token': csrf},
            data: ['onboarding_task_list_show', 'onboarding_task_list_open'].map((pref) => ({
                user_id: userId,
                category: 'onboarding_task_list',
                name: pref,
                value: 'false',
            })),
        });
        if (!prefs.ok()) {
            throw new Error(`hiding the onboarding task list in the ${name} env failed with status ${prefs.status()}`);
        }
        state.origins.push({
            origin: new URL(env.mattermost_url).origin,
            localStorage: [{name: '__landingPageSeen__', value: 'true'}],
        });
        writeFileSync(authFile(name), JSON.stringify(state));
    } finally {
        await context.dispose();
    }
}
