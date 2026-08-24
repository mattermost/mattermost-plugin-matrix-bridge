// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen} from '@testing-library/react';
import React from 'react';

import TestResultsModal from './test_results_modal';

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

describe('TestResultsModal', () => {
    it('renders skip distinctly from fail', async () => {
        mockedClient.testServer.mockResolvedValue({
            server_id: 's1',
            checks: [
                {key: 'registry', label: 'Server URL', status: 'ok', detail: 'https://a.example.com'},
                {key: 'client', label: 'Matrix Client', status: 'fail', detail: 'not initialized'},
                {key: 'connection', label: 'Connection', status: 'skip', detail: ''},
                {key: 'appservice', label: 'Application Service', status: 'skip', detail: ''},
            ],
            server_info: null,
        });

        render(
            <TestResultsModal
                server={server}
                onClose={jest.fn()}
            />,
        );

        const failItem = await screen.findByText('Matrix Client');
        const skipItems = screen.getAllByText('(skipped)', {exact: false});

        expect(failItem.closest('li')).toHaveTextContent('❌');
        expect(skipItems).toHaveLength(2);
        skipItems.forEach((item) => {
            expect(item.closest('li')).toHaveTextContent('⏭️');
        });
    });
});
