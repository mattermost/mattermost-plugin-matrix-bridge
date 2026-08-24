// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen} from '@testing-library/react';
import React from 'react';

import RemoveServerDialog from './remove_server_dialog';

import type {ServerView} from '@/types/matrix';

function buildServer(overrides: Partial<ServerView> = {}): ServerView {
    return {
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
        ...overrides,
    };
}

describe('RemoveServerDialog', () => {
    it('contains the server_id and the --server-id restore command', () => {
        const server = buildServer();
        render(
            <RemoveServerDialog
                server={server}
                onClose={jest.fn()}
                onRemoved={jest.fn()}
                onDisableInstead={jest.fn()}
            />,
        );

        expect(screen.getByText('s1')).toBeInTheDocument();
        expect(screen.getByText(/--server-id s1/)).toBeInTheDocument();
    });

    it('disables removal and offers Disable instead for a migrated server', () => {
        const server = buildServer({is_migrated: true});
        render(
            <RemoveServerDialog
                server={server}
                onClose={jest.fn()}
                onRemoved={jest.fn()}
                onDisableInstead={jest.fn()}
            />,
        );

        expect(screen.queryByRole('button', {name: 'Remove'})).not.toBeInTheDocument();
        expect(screen.getByRole('button', {name: 'Disable instead'})).toBeInTheDocument();
    });
});
