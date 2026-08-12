// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen} from '@testing-library/react';
import React from 'react';

import RegistrationModal from './registration_modal';

import * as client from '@/client';
import type {ServerView} from '@/types/matrix';

jest.mock('@/client');

const mockedClient = client as jest.Mocked<typeof client>;

const server: ServerView = {
    server_id: 's1',
    server_url: 'https://a.example.com',
    server_name: 'a.example.com',
    endpoint: 'a.example.com:443',
    event_domain: 'a_example_com_443',
    username_prefix: 'matrix',
    enabled: true,
    remote_id: 'remote1',
    is_migrated: false,
    has_as_token: true,
    has_hs_token: true,
    mapped_channel_count: 0,
};

describe('RegistrationModal', () => {
    it('renders the registration content verbatim, including no _matrix/app/v1', async () => {
        const content = 'id: mattermost-bridge-s1\nurl: https://mm.example.com/plugins/com.mattermost.plugin-matrix-bridge\nas_token: as1\nhs_token: hs1\n';
        mockedClient.getServerRegistration.mockResolvedValue({filename: 'mattermost-bridge-s1.yaml', content});

        render(
            <RegistrationModal
                server={server}
                onClose={jest.fn()}
            />,
        );

        const block = await screen.findByText((_text, node) => node?.textContent === content);
        expect(block).toBeInTheDocument();

        // The content block itself must never contain the doubled path - the
        // page's own warning text mentions it deliberately, so the check is
        // scoped to the rendered YAML, not the whole page.
        expect(block.textContent).not.toMatch(/_matrix\/app\/v1/);
    });

    it('interpolates server_name into the room_list_publication_rules snippet', async () => {
        mockedClient.getServerRegistration.mockResolvedValue({filename: 'f.yaml', content: 'id: x\n'});

        render(
            <RegistrationModal
                server={server}
                onClose={jest.fn()}
            />,
        );

        await screen.findByText(/room_list_publication_rules/);

        // Must match the sender_localpart the backend actually registers
        // (server/servers/service.go's RegistrationYAML: "_mattermost_bot"), not a
        // made-up "_mattermost_bridge" the homeserver would never match.
        expect(screen.getByText(/@_mattermost_bot:a\.example\.com/)).toBeInTheDocument();
    });
});
