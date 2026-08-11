// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen} from '@testing-library/react';
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
        mapped_channel_count: 3,
        ...overrides,
    };
}

const noop = () => undefined;
const asyncNoop = () => Promise.resolve();

describe('ServerTable', () => {
    it('renders name, URL, state, health and mapped channel count', () => {
        render(
            <ServerTable
                servers={[buildServer()]}
                countsUnavailable={false}
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

        // "Enabled" appears both as the State column's header and its cell value.
        expect(screen.getAllByText('Enabled')).toHaveLength(2);
        expect(screen.getByText('healthy')).toBeInTheDocument();
        expect(screen.getByText('3')).toBeInTheDocument();
    });

    it('renders "unavailable", not 0, when mapped_channel_count is null', () => {
        render(
            <ServerTable
                servers={[buildServer({mapped_channel_count: null})]}
                countsUnavailable={true}
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

        expect(screen.getByText('unavailable')).toBeInTheDocument();
        expect(screen.queryByText('0')).not.toBeInTheDocument();
    });

    it('disables Remove with an explanation for a migrated server', () => {
        render(
            <ServerTable
                servers={[buildServer({is_migrated: true})]}
                countsUnavailable={false}
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

        const removeButton = screen.getByRole('button', {name: 'Remove'});
        expect(removeButton).toBeDisabled();
        expect(removeButton).toHaveAttribute('title', expect.stringContaining('migrated'));
    });

    it('rolls back the enable toggle\'s optimistic state when the request fails', async () => {
        const onToggleEnabled = jest.fn().mockRejectedValue(new Error('boom'));
        render(
            <ServerTable
                servers={[buildServer({enabled: false})]}
                countsUnavailable={false}
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

        const checkbox = screen.getByRole('checkbox') as HTMLInputElement;
        expect(checkbox.checked).toBe(false);

        checkbox.click();
        expect(checkbox.checked).toBe(true); // optimistic flip

        await screen.findByRole('checkbox');
        await new Promise((resolve) => setTimeout(resolve, 0));

        expect(checkbox.checked).toBe(false); // rolled back after the rejection
    });

    it('shows an empty-state message when there are no servers', () => {
        render(
            <ServerTable
                servers={[]}
                countsUnavailable={false}
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
