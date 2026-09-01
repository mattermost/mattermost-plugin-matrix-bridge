// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen, fireEvent, waitFor, act} from '@testing-library/react';
import React from 'react';

import AddServerModal from './add_server_modal';

import * as client from '@/client';

jest.mock('@/client');

const mockedClient = client as jest.Mocked<typeof client>;

describe('AddServerModal', () => {
    it('omits empty optional fields from the request', async () => {
        mockedClient.addServer.mockResolvedValue({
            server: {
                server_id: 's1',
                server_url: 'https://a.example.com',
                server_name: 'a.example.com',
                endpoint: 'a.example.com:443',
                event_domain: 'a_example_com_443',
                username_prefix: 'matrix',
                enabled: true,
                remote_id: 'remote1',
                has_as_token: true,
                has_hs_token: true,
            },
            warnings: [],
        });

        render(
            <AddServerModal
                onClose={jest.fn()}
                onAdded={jest.fn()}
                onViewRegistration={jest.fn()}
            />,
        );

        fireEvent.change(screen.getByLabelText('Homeserver URL'), {target: {value: 'https://a.example.com'}});
        fireEvent.change(screen.getByLabelText('Application Service token'), {target: {value: 'as1'}});
        fireEvent.change(screen.getByLabelText('Homeserver token'), {target: {value: 'hs1'}});

        fireEvent.click(screen.getByRole('button', {name: 'Add server'}));

        await waitFor(() => expect(mockedClient.addServer).toHaveBeenCalled());
        expect(mockedClient.addServer).toHaveBeenCalledWith({
            server_url: 'https://a.example.com',
            as_token: 'as1',
            hs_token: 'hs1',
            username_prefix: undefined,
            server_id: undefined,
            server_name: undefined,
        });
    });

    it('regenerates a token as a v4 UUID when Regenerate is clicked', async () => {
        render(
            <AddServerModal
                onClose={jest.fn()}
                onAdded={jest.fn()}
                onViewRegistration={jest.fn()}
            />,
        );

        const asTokenInput = screen.getByLabelText('Application Service token') as HTMLInputElement;
        const hsTokenInput = screen.getByLabelText('Homeserver token') as HTMLInputElement;
        expect(asTokenInput.value).toBe('');
        expect(hsTokenInput.value).toBe('');

        fireEvent.click(screen.getAllByRole('button', {name: 'Regenerate'})[0]);
        fireEvent.click(screen.getAllByRole('button', {name: 'Regenerate'})[1]);

        const uuidRegex = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
        expect(asTokenInput.value).toMatch(uuidRegex);
        expect(hsTokenInput.value).toMatch(uuidRegex);
        expect(asTokenInput.value).not.toBe(hsTokenInput.value);
    });

    it('submits via its own button click, not a native nested-form submit, when rendered inside the System Console\'s outer <form> (regression: this section renders inline, not through a portal, so it always sits inside the admin console\'s own settings <form> - see styles.ts - and a nested <form> here previously let some browsers route the submit to that outer form instead, reloading the page and never calling addServer at all)', async () => {
        mockedClient.addServer.mockResolvedValue({
            server: {
                server_id: 's1',
                server_url: 'https://a.example.com',
                server_name: 'a.example.com',
                endpoint: 'a.example.com:443',
                event_domain: 'a_example_com_443',
                username_prefix: 'matrix',
                enabled: true,
                remote_id: 'remote1',
                has_as_token: true,
                has_hs_token: true,
            },
            warnings: [],
        });

        const outerSubmit = jest.fn((e: React.FormEvent) => e.preventDefault());

        render(
            <form onSubmit={outerSubmit}>
                <AddServerModal
                    onClose={jest.fn()}
                    onAdded={jest.fn()}
                    onViewRegistration={jest.fn()}
                />
            </form>,
        );

        fireEvent.change(screen.getByLabelText('Homeserver URL'), {target: {value: 'https://a.example.com'}});
        fireEvent.change(screen.getByLabelText('Application Service token'), {target: {value: 'as1'}});
        fireEvent.change(screen.getByLabelText('Homeserver token'), {target: {value: 'hs1'}});

        fireEvent.click(screen.getByRole('button', {name: 'Add server'}));

        await waitFor(() => expect(mockedClient.addServer).toHaveBeenCalled());
        expect(outerSubmit).not.toHaveBeenCalled();
    });

    it('surfaces a 409 message verbatim', async () => {
        mockedClient.addServer.mockRejectedValue(new Error('a server is already registered at this endpoint (server_id: s1)'));

        render(
            <AddServerModal
                onClose={jest.fn()}
                onAdded={jest.fn()}
                onViewRegistration={jest.fn()}
            />,
        );

        fireEvent.change(screen.getByLabelText('Homeserver URL'), {target: {value: 'https://a.example.com'}});
        fireEvent.change(screen.getByLabelText('Application Service token'), {target: {value: 'as1'}});
        fireEvent.change(screen.getByLabelText('Homeserver token'), {target: {value: 'hs1'}});
        fireEvent.click(screen.getByRole('button', {name: 'Add server'}));

        await screen.findByText('a server is already registered at this endpoint (server_id: s1)');
    });

    // The footer button is disabled while the request is in flight, but the
    // hand-wired Enter handler is not a button and had no such guard.
    it('ignores a second Enter while the first request is in flight', async () => {
        let resolveAdd: (v: Awaited<ReturnType<typeof client.addServer>>) => void = () => {};
        mockedClient.addServer.mockImplementation(() => new Promise((resolve) => {
            resolveAdd = resolve;
        }));

        render(
            <AddServerModal
                onClose={jest.fn()}
                onAdded={jest.fn()}
                onViewRegistration={jest.fn()}
            />,
        );

        const url = screen.getByLabelText('Homeserver URL');
        fireEvent.change(url, {target: {value: 'https://a.example.com'}});
        fireEvent.change(screen.getByLabelText('Application Service token'), {target: {value: 'as1'}});
        fireEvent.change(screen.getByLabelText('Homeserver token'), {target: {value: 'hs1'}});

        fireEvent.keyDown(url, {key: 'Enter'});
        await waitFor(() => expect(mockedClient.addServer).toHaveBeenCalledTimes(1));

        fireEvent.keyDown(url, {key: 'Enter'});
        fireEvent.keyDown(url, {key: 'Enter'});
        expect(mockedClient.addServer).toHaveBeenCalledTimes(1);

        // Settle inside act so the resulting setSubmitting(false) is not an
        // unwrapped update.
        await act(async () => {
            resolveAdd({
                server: {
                    server_id: 's1',
                    server_url: 'https://a.example.com',
                    server_name: 'a.example.com',
                    endpoint: 'a.example.com:443',
                    event_domain: 'a_example_com_443',
                    username_prefix: 'matrix',
                    enabled: true,
                    remote_id: 'remote1',
                    has_as_token: true,
                    has_hs_token: true,
                },
                warnings: [],
            });
        });
    });
});
