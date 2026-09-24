import {readFileSync} from 'node:fs';
import path from 'node:path';

// default has one registered homeserver; noServer starts with none.
export type EnvName = 'default' | 'noServer';
export const envNames: EnvName[] = ['default', 'noServer'];

// Connection details written by e2e/cmd/e2e-env.
export interface E2EEnv {
    mattermost_url: string;
    site_url: string;
    admin_username: string;
    admin_password: string;
    team_name: string;
    plugin_id: string;
    synapse_internal_url: string;
    synapse_server_name: string;
    as_token: string;
    hs_token: string;
    server_id: string;
    remote_id: string;
}

export const stateDir = path.join(__dirname, '..', '.e2e');
export const pidsFile = path.join(stateDir, 'pids.json');

export function envFile(name: EnvName): string {
    return path.join(stateDir, `${name}.json`);
}

export function readEnv(name: EnvName): E2EEnv {
    return JSON.parse(readFileSync(envFile(name), 'utf8')) as E2EEnv;
}

export function authFile(name: EnvName): string {
    return path.join(stateDir, `${name}-admin.json`);
}
