import {expect} from '@playwright/test';

import type {Api} from './fixtures';

export interface Channel {
    id: string;
    name: string;
    display_name: string;
}

function uniqueSuffix(): string {
    return `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
}

async function teamId(api: Api, teamName: string): Promise<string> {
    const resp = await api.request.get(`/api/v4/teams/name/${teamName}`);
    expect(resp.ok(), `get team ${teamName}: status ${resp.status()}`).toBe(true);
    return (await resp.json()).id;
}

// createUser creates a non-admin user who is a member of teamName.
export async function createUser(api: Api, teamName: string): Promise<{username: string; password: string}> {
    const username = `e2e-ui-${uniqueSuffix()}`;
    const password = `Pw-${uniqueSuffix()}`;
    const created = await api.request.post('/api/v4/users', {
        data: {username, password, email: `${username}@example.com`},
    });
    expect(created.ok(), `create user: status ${created.status()}`).toBe(true);
    const userId = (await created.json()).id;

    const team = await teamId(api, teamName);
    const member = await api.request.post(`/api/v4/teams/${team}/members`, {data: {team_id: team, user_id: userId}});
    expect(member.ok(), `add user to team: status ${member.status()}`).toBe(true);
    return {username, password};
}

// createChannel creates a public channel in teamName; the admin is its only member.
export async function createChannel(api: Api, teamName: string): Promise<Channel> {
    const name = `e2e-ui-${uniqueSuffix()}`;
    const resp = await api.request.post('/api/v4/channels', {
        data: {team_id: await teamId(api, teamName), name, display_name: `E2E UI ${name}`, type: 'O'},
    });
    expect(resp.ok(), `create channel: status ${resp.status()}`).toBe(true);
    return resp.json();
}

// executeCommand runs a slash command as the admin in channelId and returns its reply text.
export async function executeCommand(api: Api, channelId: string, command: string): Promise<string> {
    const resp = await api.request.post('/api/v4/commands/execute', {data: {channel_id: channelId, command}});
    const body = await resp.text();
    expect(resp.ok(), `${command}: status ${resp.status()}: ${body}`).toBe(true);
    return JSON.parse(body).text ?? '';
}
