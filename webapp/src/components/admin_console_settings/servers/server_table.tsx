// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React from 'react';

import ServerRow from './server_row';
import {emptyStateStyle} from './styles';

import type {ServerView} from '@/types/matrix';

interface Props {
    servers: ServerView[];
    health: Record<string, string>;
    loading: boolean;
    expandedServerId: string | null;
    onToggleExpand: (serverId: string) => void;
    onToggleEnabled: (server: ServerView, enabled: boolean) => Promise<void>;
    onEdit: (server: ServerView) => void;
    onRemove: (server: ServerView) => void;
    onTest: (server: ServerView) => void;
    onRegistration: (server: ServerView) => void;
}

const ServerTable: React.FC<Props> = ({
    servers,
    health,
    loading,
    expandedServerId,
    onToggleExpand,
    onToggleEnabled,
    onEdit,
    onRemove,
    onTest,
    onRegistration,
}) => {
    if (loading) {
        return (
            <p
                className='help-text'
                style={emptyStateStyle}
            >
                {'Loading Matrix servers…'}
            </p>
        );
    }

    if (servers.length === 0) {
        return (
            <div
                className='help-text'
                style={emptyStateStyle}
            >
                <p>{'No Matrix servers are registered yet. Click "Add a connection" to register your first homeserver.'}</p>
                <p>{'Bridging a channel to a homeserver is done with '}<code>{'/matrix map'}</code>{' from inside that channel, once the server is registered and its Application Service registration is installed.'}</p>
            </div>
        );
    }

    return (
        <div>
            {servers.map((server) => (
                <ServerRow
                    key={server.server_id}
                    server={server}
                    health={health[server.server_id]}
                    expanded={expandedServerId === server.server_id}
                    onToggleExpand={() => onToggleExpand(server.server_id)}
                    onToggleEnabled={onToggleEnabled}
                    onEdit={onEdit}
                    onRemove={onRemove}
                    onTest={onTest}
                    onRegistration={onRegistration}
                />
            ))}
        </div>
    );
};

export default ServerTable;
