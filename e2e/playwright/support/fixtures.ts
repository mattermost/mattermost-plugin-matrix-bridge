import {test as base, request, type APIRequestContext, type APIResponse} from '@playwright/test';

import {authFile, readEnv, type E2EEnv, type EnvName} from './env';

export interface Api {
    // request is authenticated as the admin with a bearer token, so it needs no CSRF header.
    request: APIRequestContext;

    // pluginRequest calls the plugin's REST API under /plugins/<plugin_id>/api/v1.
    pluginRequest(method: string, path: string, body?: unknown): Promise<APIResponse>;
}

interface Options {
    // envName picks the launcher environment; set it with test.use({envName: 'noServer'}).
    envName: EnvName;
}

interface Fixtures {
    env: E2EEnv;
    api: Api;
}

export const test = base.extend<Options & Fixtures>({
    envName: ['default', {option: true}],
    env: async ({envName}, use) => {
        await use(readEnv(envName));
    },
    baseURL: async ({env}, use) => {
        await use(env.mattermost_url);
    },
    storageState: async ({envName}, use) => {
        await use(authFile(envName));
    },
    api: async ({env}, use) => {
        // Contexts created inside a test inherit its storageState, and Mattermost rejects the
        // session cookie on requests without X-Requested-With, so start from an empty state.
        const noCookies = {cookies: [], origins: []};
        const login = await request.newContext({baseURL: env.mattermost_url, storageState: noCookies});
        const resp = await login.post('/api/v4/users/login', {
            data: {login_id: env.admin_username, password: env.admin_password},
        });
        const token = resp.headers().token;
        await login.dispose();
        if (!resp.ok() || !token) {
            throw new Error(`admin API login failed with status ${resp.status()}`);
        }

        const context = await request.newContext({
            baseURL: env.mattermost_url,
            storageState: noCookies,
            extraHTTPHeaders: {Authorization: `Bearer ${token}`},
        });
        await use({
            request: context,
            pluginRequest: (method, path, body) => context.fetch(`/plugins/${env.plugin_id}/api/v1${path}`, {method, data: body}),
        });
        await context.dispose();
    },
});

export {expect} from '@playwright/test';
