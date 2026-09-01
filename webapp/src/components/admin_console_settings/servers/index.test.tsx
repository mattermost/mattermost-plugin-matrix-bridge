// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen, fireEvent, waitFor, act} from '@testing-library/react';
import React from 'react';

import MatrixServersSection from './index';

import * as client from '@/client';
import type {ServerView} from '@/types/matrix';

jest.mock('@/client');

const mockedClient = client as jest.Mocked<typeof client>;

function buildServer(overrides: Partial<ServerView> = {}): ServerView {
    return {
        server_id: 'server1',
        server_url: 'https://matrix.example.com',
        server_name: 'matrix.example.com',
        endpoint: 'matrix.example.com:443',
        event_domain: 'matrix_example_com_443',
        username_prefix: 'matrix',
        enabled: true,
        remote_id: 'remote1',
        has_as_token: true,
        has_hs_token: true,
        ...overrides,
    };
}

function openActionsMenu(serverName: string) {
    fireEvent.click(screen.getByRole('button', {name: `Actions for ${serverName}`}));
}

describe('MatrixServersSection', () => {
    // A disabled server's stored health is "disabled", and pillFor reads health
    // ahead of `enabled`. Refreshing only the server list after the mutation left
    // that stale "disabled" reading in place, so the row kept saying Disabled
    // after a successful enable.
    it('re-probes health after an enable, so the row stops reading Disabled', async () => {
        mockedClient.setServerEnabled.mockResolvedValue({server: buildServer({enabled: true})});

        // Disabled to start, with the matching "disabled" health reading.
        mockedClient.listServers.mockResolvedValue({servers: [buildServer({enabled: false})]});
        mockedClient.getServersHealth.mockResolvedValue({health: {server1: 'disabled'}});

        render(<MatrixServersSection/>);

        expect(await screen.findByText('Disabled')).toBeInTheDocument();

        // What the server returns once it is enabled again.
        mockedClient.listServers.mockResolvedValue({servers: [buildServer({enabled: true})]});
        mockedClient.getServersHealth.mockResolvedValue({health: {server1: 'healthy'}});

        openActionsMenu('matrix.example.com');
        fireEvent.click(screen.getByRole('menuitem', {name: /Enable connection/}));

        await waitFor(() => expect(mockedClient.setServerEnabled).toHaveBeenCalledWith('server1', true));

        expect(await screen.findByText('Active')).toBeInTheDocument();
        expect(screen.queryByText('Disabled')).not.toBeInTheDocument();

        // Twice each: once on mount, once after the mutation.
        expect(mockedClient.getServersHealth).toHaveBeenCalledTimes(2);

        // Settle the row's remaining post-mutation state before teardown.
        await act(async () => {});
    });

    // Health probes fan out over the network and the server bounds a round at 8s, so
    // two rounds overlap easily - every mutation starts one. Without a request-ID
    // guard the slower, older round lands last and overwrites the newer readings.
    it('ignores a health round that resolves after a newer one', async () => {
        mockedClient.listServers.mockResolvedValue({servers: [buildServer()]});
        mockedClient.setServerEnabled.mockResolvedValue({server: buildServer()});

        const resolvers: Array<(v: {health: Record<string, string>}) => void> = [];
        mockedClient.getServersHealth.mockImplementation(() => new Promise((resolve) => {
            resolvers.push(resolve);
        }));

        render(<MatrixServersSection/>);
        await waitFor(() => expect(resolvers).toHaveLength(1));

        // A second round starts before the first has resolved.
        fireEvent.click(screen.getByRole('button', {name: 'Refresh'}));
        await waitFor(() => expect(resolvers).toHaveLength(2));

        // The NEWER round resolves first, then the older one - out of order.
        await act(async () => {
            resolvers[1]({health: {server1: 'healthy'}});
        });
        await act(async () => {
            resolvers[0]({health: {server1: 'unhealthy'}});
        });

        expect(await screen.findByText('Active')).toBeInTheDocument();
        expect(screen.queryByText('Unhealthy')).not.toBeInTheDocument();
    });
});
