import {request, type APIRequestContext} from '@playwright/test';

export type StorageState = Awaited<ReturnType<APIRequestContext['storageState']>>;

export const emptyStorageState: StorageState = {cookies: [], origins: []};

// loginStorageState logs a user in through the API and returns the session cookies as a browser
// storage state. It also marks the desktop-app landing page as seen (a localStorage flag), which
// Mattermost otherwise shows on a browser's first visit.
export async function loginStorageState(baseURL: string, loginId: string, password: string): Promise<StorageState> {
    // Contexts created inside a test inherit its storageState, so start from an empty one.
    const context = await request.newContext({baseURL, storageState: emptyStorageState});
    try {
        const resp = await context.post('/api/v4/users/login', {
            headers: {'X-Requested-With': 'XMLHttpRequest'},
            data: {login_id: loginId, password},
        });
        if (!resp.ok()) {
            throw new Error(`login as ${loginId} failed with status ${resp.status()}`);
        }
        const state = await context.storageState();
        state.origins.push({
            origin: new URL(baseURL).origin,
            localStorage: [{name: '__landingPageSeen__', value: 'true'}],
        });
        return state;
    } finally {
        await context.dispose();
    }
}
