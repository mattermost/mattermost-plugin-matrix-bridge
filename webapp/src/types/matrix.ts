// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

// These mirror the plugin REST API's JSON wire format exactly (server/api_servers.go
// and server/servers/service.go), snake_case included, so the client module needs no
// mapping layer between the two.

export interface ServerView {
    server_id: string;
    server_url: string;
    server_name: string;
    endpoint: string;
    event_domain: string;
    username_prefix: string;
    enabled: boolean;
    remote_id: string;
    is_migrated: boolean;
    has_as_token: boolean;
    has_hs_token: boolean;
}

export interface ListServersResponse {
    servers: ServerView[];
}

export interface AddServerRequest {
    server_url: string;
    as_token: string;
    hs_token: string;
    username_prefix?: string;
    server_id?: string; // restore a previously removed server
    server_name?: string; // override discovery
}

export interface AddServerResponse {
    server: ServerView;
    warnings: string[];
}

// A field absent here means "leave alone" - the caller must omit a blank token
// input rather than send "", which the API rejects.
export interface UpdateServerRequest {
    server_url?: string;
    as_token?: string;
    hs_token?: string;
    username_prefix?: string;
    server_name?: string;
}

export interface UpdateServerResponse {
    server: ServerView;
    warnings: string[];
}

export interface RemoveServerResponse {
    server_id: string;
    recovery_command: string;
}

export interface SetServerEnabledResponse {
    server: ServerView;
}

export interface ServersHealthResponse {
    health: Record<string, string>;
}

export type DiagnosticStatus = 'ok' | 'fail' | 'skip';

export interface DiagnosticCheck {
    key: string;
    label: string;
    status: DiagnosticStatus;
    detail: string;
}

export interface MatrixServerInfo {
    name: string;
    version: string;
}

export interface Diagnostics {
    server_id: string;
    checks: DiagnosticCheck[];
    server_info: MatrixServerInfo | null;
}

export interface RegistrationResponse {
    filename: string;
    content: string;
}

export interface MappingView {
    channel_id: string;
    channel_name: string;
    team_name: string; // "" for a DM/GM - render "Direct message"
    room_id: string;
    channel_missing: boolean;
}

export interface MappingsResponse {
    total_count: number;
    mappings: MappingView[];
}

export interface APIErrorBody {
    message: string;
}
