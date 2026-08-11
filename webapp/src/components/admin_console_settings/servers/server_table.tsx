// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React from 'react';

import ServerRow from './server_row';

import type {ServerView} from '@/types/matrix';

interface Props {
    servers: ServerView[];
    countsUnavailable: boolean;
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
    countsUnavailable,
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
        return <p>{'Loading Matrix servers…'}</p>;
    }

    if (servers.length === 0) {
        return (
            <div className='help-text'>
                <p>{'No Matrix servers are registered yet. Click "Add Matrix server" to register your first homeserver.'}</p>
                <p>{'Bridging a channel to a homeserver is done with '}<code>{'/matrix map'}</code>{' from inside that channel, once the server is registered and its Application Service registration is installed.'}</p>
            </div>
        );
    }

    return (
        <>
            {countsUnavailable && (
                <div
                    className='help-text'
                    style={{color: '#a94442'}}
                >
                    {'Mapped-channel counts are temporarily unavailable; they are shown as "unavailable" below rather than 0.'}
                </div>
            )}
            <table className='table'>
                <thead>
                    <tr>
                        <th>{'Name'}</th>
                        <th>{'URL'}</th>
                        <th>{'State'}</th>
                        <th>{'Health'}</th>
                        <th>{'Mapped channels'}</th>
                        <th>{'Enabled'}</th>
                        <th>{'Actions'}</th>
                    </tr>
                </thead>
                <tbody>
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
                </tbody>
            </table>
        </>
    );
};

export default ServerTable;
