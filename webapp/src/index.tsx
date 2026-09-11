// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import type {Store, Action} from 'redux';

import type {GlobalState} from '@mattermost/types/store';

import MatrixServersSection from '@/components/admin_console_settings/servers';
import manifest from '@/manifest';
import type {PluginRegistry} from '@/types/mattermost-webapp';

export default class Plugin {
    // eslint-disable-next-line @typescript-eslint/no-unused-vars, @typescript-eslint/no-empty-function
    public async initialize(registry: PluginRegistry, store: Store<GlobalState, Action<Record<string, unknown>>>) {
        // Homeserver management lives in the System Console section registered
        // below (server/servers, plugin.json's "matrix_servers" section) and in
        // the /matrix server slash command - not in the two DOM-scraping
        // registration_download / homeserver_config settings this replaced.
        registry.registerAdminConsoleCustomSection('matrix_servers', MatrixServersSection);
    }
}

declare global {
    interface Window {
        registerPlugin(pluginId: string, plugin: Plugin): void;
    }
}

window.registerPlugin(manifest.id, new Plugin());
