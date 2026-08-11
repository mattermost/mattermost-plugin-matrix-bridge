// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen, fireEvent, waitFor, within} from '@testing-library/react';
import React from 'react';

import MappingsPanel from './mappings_panel';

import * as client from '@/client';

jest.mock('@/client');

const mockedClient = client as jest.Mocked<typeof client>;

describe('MappingsPanel', () => {
    it('paginates using page/per_page', async () => {
        mockedClient.getServerMappings.mockResolvedValue({
            total_count: 60,
            mappings: [{channel_id: 'c1', channel_name: 'Town Square', team_name: 'core', room_id: '!room:example.com', channel_missing: false}],
        });

        render(<MappingsPanel serverId='s1'/>);

        await screen.findByText('Town Square');
        expect(mockedClient.getServerMappings).toHaveBeenCalledWith('s1', 0, 50);

        fireEvent.click(screen.getByRole('button', {name: 'Next'}));

        await waitFor(() => expect(mockedClient.getServerMappings).toHaveBeenCalledWith('s1', 1, 50));
    });

    it('labels a channel_missing row as deleted and still allows unmapping it', async () => {
        mockedClient.getServerMappings.mockResolvedValue({
            total_count: 1,
            mappings: [{channel_id: 'gone', channel_name: '', team_name: '', room_id: '!room:example.com', channel_missing: true}],
        });
        mockedClient.unmapServerChannel.mockResolvedValue(undefined);

        render(<MappingsPanel serverId='s1'/>);

        await screen.findByText('Channel deleted');

        fireEvent.click(screen.getByRole('button', {name: 'Unmap'}));

        const dialog = screen.getByRole('dialog');
        fireEvent.click(within(dialog).getByRole('button', {name: 'Unmap'}));

        await waitFor(() => expect(mockedClient.unmapServerChannel).toHaveBeenCalledWith('s1', 'gone'));
    });
});
