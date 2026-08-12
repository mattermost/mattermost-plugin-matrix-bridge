// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useState} from 'react';

import MappingsPanel from './mappings_panel';

import type {ServerView} from '@/types/matrix';

interface Props {
    server: ServerView;
    health?: string;
    expanded: boolean;
    onToggleExpand: () => void;
    onToggleEnabled: (server: ServerView, enabled: boolean) => Promise<void>;
    onEdit: (server: ServerView) => void;
    onRemove: (server: ServerView) => void;
    onTest: (server: ServerView) => void;
    onRegistration: (server: ServerView) => void;
}

const COLUMN_COUNT = 7;

function copyToClipboard(text: string) {
    navigator.clipboard?.writeText(text).catch(() => {
        // Best-effort only - the ID is still visible and selectable by hand.
    });
}

const ServerRow: React.FC<Props> = ({server, health, expanded, onToggleExpand, onToggleEnabled, onEdit, onRemove, onTest, onRegistration}) => {
    const [enabling, setEnabling] = useState(false);
    const [optimisticEnabled, setOptimisticEnabled] = useState<boolean | null>(null);

    const enabled = optimisticEnabled === null ? server.enabled : optimisticEnabled;

    const handleToggle = async () => {
        const next = !enabled;
        setOptimisticEnabled(next);
        setEnabling(true);
        try {
            await onToggleEnabled(server, next);

            // Success - defer back to server.enabled (via the refreshed prop) instead
            // of staying pinned to the optimistic value, so a later external change
            // (e.g. another admin toggling it) is reflected instead of masked.
            setOptimisticEnabled(null);
        } catch (e) {
            // Roll back the optimistic flip - the mutation failed server-side.
            setOptimisticEnabled(!next);
        } finally {
            setEnabling(false);
        }
    };

    const mappedChannelCountText = server.mapped_channel_count === null ? 'unavailable' : String(server.mapped_channel_count);

    return (
        <>
            <tr>
                <td>
                    <div>{server.server_name}</div>
                    <button
                        type='button'
                        className='style--none'
                        style={{fontFamily: 'monospace', fontSize: '12px', color: '#666', background: 'none', border: 'none', padding: 0, cursor: 'pointer'}}
                        title='Copy server ID'
                        onClick={() => copyToClipboard(server.server_id)}
                    >
                        {server.server_id}
                    </button>
                    {server.is_migrated && (
                        <div style={{fontSize: '12px', color: '#666'}}>{'Migrated from legacy configuration'}</div>
                    )}
                </td>
                <td>{server.server_url}</td>
                <td>{enabled ? 'Enabled' : 'Disabled'}</td>
                <td>{health || 'unknown'}</td>
                <td>{mappedChannelCountText}</td>
                <td>
                    <label
                        style={{display: 'inline-flex', alignItems: 'center', cursor: enabling ? 'wait' : 'pointer'}}
                        title='Disabling stops sync without touching mappings, ghosts or the shared-channels remote'
                    >
                        <input
                            type='checkbox'
                            aria-label={enabled ? `Disable ${server.server_name}` : `Enable ${server.server_name}`}
                            checked={enabled}
                            disabled={enabling}
                            onChange={handleToggle}
                        />
                    </label>
                </td>
                <td>
                    <div style={{display: 'flex', gap: '6px', flexWrap: 'wrap'}}>
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            onClick={() => onTest(server)}
                        >
                            {'Test'}
                        </button>
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            onClick={() => onRegistration(server)}
                        >
                            {'Registration'}
                        </button>
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            onClick={onToggleExpand}
                        >
                            {expanded ? 'Hide mappings' : 'Mappings'}
                        </button>
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            onClick={() => onEdit(server)}
                        >
                            {'Edit'}
                        </button>
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            disabled={server.is_migrated}
                            title={server.is_migrated ? 'This server was migrated from the legacy configuration and cannot be removed - use the toggle to disable it instead' : undefined}
                            onClick={() => onRemove(server)}
                        >
                            {'Remove'}
                        </button>
                    </div>
                </td>
            </tr>
            {expanded && (
                <tr>
                    <td colSpan={COLUMN_COUNT}>
                        <MappingsPanel serverId={server.server_id}/>
                    </td>
                </tr>
            )}
        </>
    );
};

export default ServerRow;
