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

        // Settle the row's remaining post-mutation state.
        //
        // This emits React's "state update on an unmounted component" warning,
        // which is pre-existing and unrelated to the health re-probe: refresh()
        // sets `loading`, and ServerTable swaps every row for a "Loading Matrix
        // servers…" message while it is true, so each toggle unmounts the row
        // that is still awaiting its own handleToggle. The warning goes away by
        // keeping the rows mounted across a refresh, not by changing this test.
        await act(async () => {});
    });
});
