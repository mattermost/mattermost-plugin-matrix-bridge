// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import * as client from '@/client';
import manifest from '@/manifest';

function jsonResponse(status: number, body: unknown): Response {
    return {
        ok: status >= 200 && status < 300,
        status,
        statusText: 'Status Text',
        json: () => Promise.resolve(body),
    } as Response;
}

describe('client', () => {
    let fetchMock: jest.Mock;

    beforeEach(() => {
        // jsdom in this jest version has no global fetch, so it must be assigned
        // (not spied on) before each test.
        fetchMock = jest.fn();
        global.fetch = fetchMock as unknown as typeof fetch;
    });

    it('builds URLs from Client4.getUrl() + /plugins/<id>/api/v1 - never Client4.getPluginRoute', async () => {
        fetchMock.mockResolvedValue(jsonResponse(200, {servers: []}));

        await client.listServers();

        expect(fetchMock).toHaveBeenCalledTimes(1);
        const [url] = fetchMock.mock.calls[0];

        // Client4.getPluginRoute(id) resolves under /api/v4/plugins/<id> - Mattermost's
        // own plugin-management API, not where this plugin's ServeHTTP is mounted. A
        // request built from it 404s in production even though it also happens to
        // contain "/plugins/<id>/api/v1/servers" as a substring, which is why this
        // asserts the full path rather than just that substring.
        expect(String(url)).toMatch(new RegExp(`/plugins/${manifest.id}/api/v1/servers$`));
        expect(String(url)).not.toContain('/api/v4/plugins');
    });

    it('sends Client4.getOptions-derived headers, including X-Requested-With', async () => {
        fetchMock.mockResolvedValue(jsonResponse(200, {servers: []}));

        await client.listServers();

        const [, options] = fetchMock.mock.calls[0];
        const headers = (options as RequestInit).headers as Record<string, string>;
        expect(headers['X-Requested-With']).toBe('XMLHttpRequest');
    });

    it('throws an Error carrying message from a non-2xx JSON body', async () => {
        fetchMock.mockResolvedValue(jsonResponse(409, {message: 'a server is already registered at this endpoint'}));

        await expect(client.addServer({server_url: 'https://a.example.com', as_token: 'as', hs_token: 'hs'})).
            rejects.toThrow('a server is already registered at this endpoint');
    });

    it('falls back to status text for a non-JSON error body', async () => {
        const response = {
            ok: false,
            status: 502,
            statusText: 'Bad Gateway',
            json: () => Promise.reject(new Error('not json')),
        } as unknown as Response;
        fetchMock.mockResolvedValue(response);

        await expect(client.listServers()).rejects.toThrow('Bad Gateway');
    });

    it('sends a JSON-encoded body and the right method for a mutation', async () => {
        fetchMock.mockResolvedValue(jsonResponse(201, {server: {}, warnings: []}));

        await client.addServer({server_url: 'https://a.example.com', as_token: 'as', hs_token: 'hs'});

        const [, options] = fetchMock.mock.calls[0];
        const init = options as RequestInit;
        expect(init.method).toBe('POST');
        expect(JSON.parse(init.body as string)).toEqual({server_url: 'https://a.example.com', as_token: 'as', hs_token: 'hs'});
    });
});
