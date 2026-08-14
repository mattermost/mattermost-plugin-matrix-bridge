// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {Client4} from 'mattermost-redux/client';

import manifest from '@/manifest';
import type {
    AddServerRequest,
    AddServerResponse,
    ListServersResponse,
    MappingsResponse,
    RegistrationResponse,
    RemoveServerResponse,
    ServersHealthResponse,
    SetServerEnabledResponse,
    UpdateServerRequest,
    UpdateServerResponse,
    Diagnostics,
} from '@/types/matrix';

// Client4.doFetch is protected and therefore unusable from plugin code. This module
// uses window.fetch with Client4.getOptions({method, body}) instead, which is public
// and supplies credentials, the X-Requested-With header and the CSRF token that the
// plugin's MattermostAuthorizationRequired middleware needs for cookie-authenticated
// non-GET requests. A request missing them fails with a 401 that reads as "not logged
// in" - if you hit that, it's this, not an auth bug.
//
// Client4.getPluginRoute(id) resolves to "<url>/api/v4/plugins/<id>" - that is
// Mattermost's OWN REST API for managing plugins (install/enable/disable), not
// where a plugin's ServeHTTP is mounted. A plugin's own HTTP routes live at
// "<url>/plugins/<id>/..." instead, so this builds the base route from
// Client4.getUrl() directly.
const baseRoute = () => `${Client4.getUrl()}/plugins/${manifest.id}/api/v1`;

async function errorMessageFrom(response: Response): Promise<string> {
    try {
        const body = await response.json();
        if (body && typeof body.message === 'string' && body.message) {
            return body.message;
        }
    } catch (e) {
        // Not JSON (e.g. a proxy's HTML error page) - fall through to status text.
    }
    return response.statusText || `Request failed with status ${response.status}`;
}

interface RequestOptions {
    method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE';
    body?: unknown;
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    const fetchOptions = Client4.getOptions({
        method: options.method || 'GET',
        body: options.body === undefined ? undefined : JSON.stringify(options.body),
    });

    const response = await fetch(`${baseRoute()}${path}`, fetchOptions as RequestInit);

    if (!response.ok) {
        throw new Error(await errorMessageFrom(response));
    }

    if (response.status === 204) {
        return undefined as unknown as T;
    }

    return response.json() as Promise<T>;
}

export function listServers(): Promise<ListServersResponse> {
    return request<ListServersResponse>('/servers');
}

export function getServersHealth(): Promise<ServersHealthResponse> {
    return request<ServersHealthResponse>('/servers/health');
}

export function addServer(req: AddServerRequest): Promise<AddServerResponse> {
    return request<AddServerResponse>('/servers', {method: 'POST', body: req});
}

export function updateServer(serverId: string, req: UpdateServerRequest): Promise<UpdateServerResponse> {
    return request<UpdateServerResponse>(`/servers/${encodeURIComponent(serverId)}`, {method: 'PATCH', body: req});
}

export function removeServer(serverId: string): Promise<RemoveServerResponse> {
    return request<RemoveServerResponse>(`/servers/${encodeURIComponent(serverId)}`, {method: 'DELETE'});
}

export function setServerEnabled(serverId: string, enabled: boolean): Promise<SetServerEnabledResponse> {
    return request<SetServerEnabledResponse>(`/servers/${encodeURIComponent(serverId)}/enabled`, {method: 'PUT', body: {enabled}});
}

export function testServer(serverId: string): Promise<Diagnostics> {
    return request<Diagnostics>(`/servers/${encodeURIComponent(serverId)}/test`, {method: 'POST'});
}

export function getServerRegistration(serverId: string): Promise<RegistrationResponse> {
    return request<RegistrationResponse>(`/servers/${encodeURIComponent(serverId)}/registration`);
}

export function getServerMappings(serverId: string, page = 0, perPage = 50): Promise<MappingsResponse> {
    return request<MappingsResponse>(`/servers/${encodeURIComponent(serverId)}/mappings?page=${page}&per_page=${perPage}`);
}
