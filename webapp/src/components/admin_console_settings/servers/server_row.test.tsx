// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen, fireEvent} from '@testing-library/react';
import React from 'react';

import ServerRow from './server_row';

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
};

function renderRow(expanded: boolean, onToggleExpand: () => void = jest.fn()) {
    return render(
        <ServerRow
            server={server}
            expanded={expanded}
            onToggleExpand={onToggleExpand}
            onToggleEnabled={jest.fn().mockResolvedValue(undefined)}
            onEdit={jest.fn()}
            onRemove={jest.fn()}
            onTest={jest.fn()}
            onRegistration={jest.fn()}
        />,
    );
}

describe('ServerRow mappings panel', () => {
    it('does not fetch mappings until the row is expanded', () => {
        renderRow(false);
        expect(mockedClient.getServerMappings).not.toHaveBeenCalled();
    });

    it('fetches mappings once the row is expanded', async () => {
        mockedClient.getServerMappings.mockResolvedValue({total_count: 0, mappings: []});
        renderRow(true);
        await screen.findByText(/No channels are bridged/);
        expect(mockedClient.getServerMappings).toHaveBeenCalledWith('s1', 0, 50);
    });
});

describe('ServerRow channels-shared toggle', () => {
    it('clicking the "channels shared" cell toggles the row, not just a kebab-menu item', () => {
        const onToggleExpand = jest.fn();
        renderRow(false, onToggleExpand);

        fireEvent.click(screen.getByRole('button', {name: 'Show bridged channels for a.example.com'}));

        expect(onToggleExpand).toHaveBeenCalledTimes(1);
    });

    it('reflects the expanded state in its label and aria-expanded', async () => {
        mockedClient.getServerMappings.mockResolvedValue({total_count: 0, mappings: []});
        renderRow(true);

        const toggle = await screen.findByRole('button', {name: 'Hide bridged channels for a.example.com'});
        expect(toggle).toHaveAttribute('aria-expanded', 'true');

        // Let the row's own mappings fetch settle before the test ends, so its
        // resolution doesn't land outside act() after this test has moved on.
        await screen.findByText(/No channels are bridged/);
    });

    it('no longer offers a redundant "bridged channels" entry in the kebab menu', () => {
        renderRow(false);

        fireEvent.click(screen.getByRole('button', {name: 'Actions for a.example.com'}));

        expect(screen.queryByRole('menuitem', {name: /bridged channels/i})).not.toBeInTheDocument();
    });
});
