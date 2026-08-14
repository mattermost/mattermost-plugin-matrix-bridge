// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen, fireEvent, waitFor} from '@testing-library/react';
import React from 'react';

import EditServerModal from './edit_server_modal';

import * as client from '@/client';
import type {ServerView} from '@/types/matrix';

jest.mock('@/client');

const mockedClient = client as jest.Mocked<typeof client>;

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
        mapped_channel_count: 0,
        ...overrides,
    };
}

describe('EditServerModal', () => {
    it('omits blank token inputs from the PATCH body', async () => {
        const server = buildServer();
        mockedClient.updateServer.mockResolvedValue({server, warnings: []});

        render(
            <EditServerModal
                server={server}
                onClose={jest.fn()}
                onUpdated={jest.fn()}
            />,
        );

        // Leave both token fields blank and just save.
        fireEvent.click(screen.getByRole('button', {name: 'Save'}));

        await waitFor(() => expect(mockedClient.updateServer).toHaveBeenCalled());
        const [, body] = mockedClient.updateServer.mock.calls[0];
        expect(body.as_token).toBeUndefined();
        expect(body.hs_token).toBeUndefined();
    });

    it('sends a regenerated v4 UUID token in the PATCH body', async () => {
        const server = buildServer();
        mockedClient.updateServer.mockResolvedValue({server, warnings: []});

        render(
            <EditServerModal
                server={server}
                onClose={jest.fn()}
                onUpdated={jest.fn()}
            />,
        );

        fireEvent.click(screen.getAllByRole('button', {name: 'Regenerate'})[0]);
        fireEvent.click(screen.getByRole('button', {name: 'Save'}));

        await waitFor(() => expect(mockedClient.updateServer).toHaveBeenCalled());
        const [, body] = mockedClient.updateServer.mock.calls[0];
        expect(body.as_token).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    });

    it('blocks the server_name change until the confirm checkbox is checked', async () => {
        const server = buildServer();
        mockedClient.updateServer.mockResolvedValue({server, warnings: []});

        render(
            <EditServerModal
                server={server}
                onClose={jest.fn()}
                onUpdated={jest.fn()}
            />,
        );

        fireEvent.click(screen.getByRole('button', {name: 'Show advanced'}));
        fireEvent.change(screen.getByLabelText('Server name'), {target: {value: 'renamed.example.com'}});

        fireEvent.click(screen.getByRole('button', {name: 'Save'}));
        expect(mockedClient.updateServer).not.toHaveBeenCalled();
        expect(screen.getByText(/Check the confirmation box/)).toBeInTheDocument();

        fireEvent.click(screen.getByRole('checkbox'));
        fireEvent.click(screen.getByRole('button', {name: 'Save'}));

        await waitFor(() => expect(mockedClient.updateServer).toHaveBeenCalled());
        const [, body] = mockedClient.updateServer.mock.calls[0];
        expect(body.server_name).toBe('renamed.example.com');
    });

    it('renders returned warnings after a successful save', async () => {
        const server = buildServer();
        mockedClient.updateServer.mockResolvedValue({server, warnings: ['The username prefix only applies going forward.']});

        render(
            <EditServerModal
                server={server}
                onClose={jest.fn()}
                onUpdated={jest.fn()}
            />,
        );

        fireEvent.click(screen.getByRole('button', {name: 'Save'}));

        await screen.findByText('The username prefix only applies going forward.');
    });
});
