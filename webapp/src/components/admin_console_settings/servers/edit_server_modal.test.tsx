// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {render, screen, fireEvent, waitFor, act} from '@testing-library/react';
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
        has_as_token: true,
        has_hs_token: true,
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

    it('submits via its own button click, not a native nested-form submit, when rendered inside the System Console\'s outer <form> (regression: this section renders inline, not through a portal, so it always sits inside the admin console\'s own settings <form> - see styles.ts - and a nested <form> here previously let some browsers route the submit to that outer form instead, reloading the page and never calling updateServer at all)', async () => {
        const server = buildServer();
        mockedClient.updateServer.mockResolvedValue({server, warnings: []});

        const outerSubmit = jest.fn((e: React.FormEvent) => e.preventDefault());

        render(
            <form onSubmit={outerSubmit}>
                <EditServerModal
                    server={server}
                    onClose={jest.fn()}
                    onUpdated={jest.fn()}
                />
            </form>,
        );

        fireEvent.click(screen.getByRole('button', {name: 'Save'}));

        await waitFor(() => expect(mockedClient.updateServer).toHaveBeenCalled());
        expect(outerSubmit).not.toHaveBeenCalled();
    });

    it('re-opens the advanced section if it was collapsed again when the confirm checkbox blocks save', async () => {
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
        fireEvent.click(screen.getByRole('button', {name: 'Hide advanced'}));

        // The confirm checkbox is now hidden with the rest of "advanced" - saving
        // must bring it back into view rather than pointing at an invisible control.
        expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();

        fireEvent.click(screen.getByRole('button', {name: 'Save'}));

        expect(mockedClient.updateServer).not.toHaveBeenCalled();
        expect(screen.getByRole('checkbox')).toBeInTheDocument();
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

    // The footer button is disabled while the request is in flight, but the
    // hand-wired Enter handler is not a button and had no such guard.
    it('ignores a second Enter while the first request is in flight', async () => {
        const server = buildServer();
        let resolveUpdate: (v: Awaited<ReturnType<typeof client.updateServer>>) => void = () => {};
        mockedClient.updateServer.mockImplementation(() => new Promise((resolve) => {
            resolveUpdate = resolve;
        }));

        render(
            <EditServerModal
                server={server}
                onClose={jest.fn()}
                onUpdated={jest.fn()}
            />,
        );

        const url = screen.getByLabelText('Homeserver URL');

        fireEvent.keyDown(url, {key: 'Enter'});
        await waitFor(() => expect(mockedClient.updateServer).toHaveBeenCalledTimes(1));

        fireEvent.keyDown(url, {key: 'Enter'});
        fireEvent.keyDown(url, {key: 'Enter'});
        expect(mockedClient.updateServer).toHaveBeenCalledTimes(1);

        // Settle inside act so the resulting setSubmitting(false) is not an
        // unwrapped update.
        await act(async () => {
            resolveUpdate({server, warnings: []});
        });
    });
});
