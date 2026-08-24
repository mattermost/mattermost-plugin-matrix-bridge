// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen, fireEvent} from '@testing-library/react';
import React from 'react';

import ServerTable from './server_table';

import type {ServerView} from '@/types/matrix';

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
        is_migrated: false,
        has_as_token: true,
        has_hs_token: true,
        ...overrides,
    };
}

const noop = () => undefined;
const asyncNoop = () => Promise.resolve();

function openActionsMenu(serverName: string) {
    fireEvent.click(screen.getByRole('button', {name: `Actions for ${serverName}`}));
}

describe('ServerTable', () => {
    it('renders name, URL and status pill', () => {
        render(
            <ServerTable
                servers={[buildServer()]}
                health={{server1: 'healthy'}}
                loading={false}
                expandedServerId={null}
                onToggleExpand={noop}
                onToggleEnabled={asyncNoop}
                onEdit={noop}
                onRemove={noop}
                onTest={noop}
                onRegistration={noop}
            />,
        );

        expect(screen.getByText('matrix.example.com')).toBeInTheDocument();
        expect(screen.getByText('https://matrix.example.com')).toBeInTheDocument();
        expect(screen.getByText('Active')).toBeInTheDocument();
        expect(screen.getByText('Channels shared')).toBeInTheDocument();
    });

    it('disables Remove with an explanation for a migrated server', async () => {
        render(
            <ServerTable
                servers={[buildServer({is_migrated: true})]}
                health={{}}
                loading={false}
                expandedServerId={null}
                onToggleExpand={noop}
                onToggleEnabled={asyncNoop}
                onEdit={noop}
                onRemove={noop}
                onTest={noop}
                onRegistration={noop}
            />,
        );

        openActionsMenu('matrix.example.com');

        // The disabled item's own "cannot be removed" title text ends up folded
        // into its computed accessible name alongside "Remove", so match on
        // text content directly rather than the role's `name` option.
        const removeItem = screen.getAllByRole('menuitem').find((item) => item.textContent === 'Remove');
        expect(removeItem).toBeDisabled();
        expect(removeItem).toHaveAttribute('title', expect.stringContaining('migrated'));
    });

    it('rolls back the enable toggle\'s optimistic state when the request fails', async () => {
        // The rejection is held back (not mockRejectedValue, which settles on the same
        // tick) so the "optimistic flip" assertion below genuinely observes the
        // in-flight state rather than racing straight past it to the rolled-back one.
        let rejectToggle!: (reason?: unknown) => void;
        const onToggleEnabled = jest.fn(() => new Promise<void>((_resolve, reject) => {
            rejectToggle = reject;
        }));
        render(
            <ServerTable
                servers={[buildServer({enabled: false})]}
                health={{}}
                loading={false}
                expandedServerId={null}
                onToggleExpand={noop}
                onToggleEnabled={onToggleEnabled}
                onEdit={noop}
                onRemove={noop}
                onTest={noop}
                onRegistration={noop}
            />,
        );

        expect(screen.getByText('Disabled')).toBeInTheDocument();

        openActionsMenu('matrix.example.com');
        fireEvent.click(screen.getByRole('menuitem', {name: /Enable connection/}));

        // Optimistic flip - the row now reads Active immediately.
        expect(await screen.findByText('Active')).toBeInTheDocument();

        // Rolled back after the rejection.
        rejectToggle(new Error('boom'));
        expect(await screen.findByText('Disabled')).toBeInTheDocument();
    });

    it('renders Disabled, not Active, for a disabled server with a stale healthy reading', () => {
        // Health is fetched separately (GET /servers/health) and isn't
        // auto-refreshed, so it can still say "healthy" for a server that was
        // disabled since the last health probe. `enabled` must win.
        render(
            <ServerTable
                servers={[buildServer({enabled: false})]}
                health={{server1: 'healthy'}}
                loading={false}
                expandedServerId={null}
                onToggleExpand={noop}
                onToggleEnabled={asyncNoop}
                onEdit={noop}
                onRemove={noop}
                onTest={noop}
                onRegistration={noop}
            />,
        );

        expect(screen.getByText('Disabled')).toBeInTheDocument();
        expect(screen.queryByText('Active')).not.toBeInTheDocument();
    });

    it('shows an empty-state message when there are no servers', () => {
        render(
            <ServerTable
                servers={[]}
                health={{}}
                loading={false}
                expandedServerId={null}
                onToggleExpand={noop}
                onToggleEnabled={asyncNoop}
                onEdit={noop}
                onRemove={noop}
                onTest={noop}
                onRegistration={noop}
            />,
        );

        expect(screen.getByText(/No Matrix servers are registered yet/)).toBeInTheDocument();
    });
});
