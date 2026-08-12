// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen} from '@testing-library/react';
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
    mapped_channel_count: 0,
};

function renderRow(expanded: boolean) {
    return render(
        <ServerRow
            server={server}
            expanded={expanded}
            onToggleExpand={jest.fn()}
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
