// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useCallback, useEffect, useState} from 'react';

import ModalShell from './modal_shell';

import * as client from '@/client';
import type {MappingView} from '@/types/matrix';

const PER_PAGE = 50;

function messageFrom(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
}

interface Props {
    serverId: string;
}

// MappingsPanel is mounted only while its server row is expanded (see ServerRow),
// which is what makes this "lazy-loaded on open" - it fetches on mount, never as
// part of the server list render. Every fetch here is a full keyspace scan
// server-side, so it must never be triggered automatically or on an interval.
const MappingsPanel: React.FC<Props> = ({serverId}) => {
    const [mappings, setMappings] = useState<MappingView[]>([]);
    const [totalCount, setTotalCount] = useState(0);
    const [page, setPage] = useState(0);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);
    const [confirmUnmap, setConfirmUnmap] = useState<MappingView | null>(null);
    const [unmapping, setUnmapping] = useState(false);
    const [unmapError, setUnmapError] = useState<string | null>(null);

    const load = useCallback(async (targetPage: number) => {
        setLoading(true);
        try {
            const resp = await client.getServerMappings(serverId, targetPage, PER_PAGE);
            setMappings(resp.mappings);
            setTotalCount(resp.total_count);
            setPage(targetPage);
            setError(null);
        } catch (e) {
            setError(messageFrom(e));
        } finally {
            setLoading(false);
        }
    }, [serverId]);

    useEffect(() => {
        load(0);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [serverId]);

    const handleUnmap = async () => {
        if (!confirmUnmap) {
            return;
        }
        setUnmapping(true);
        try {
            await client.unmapServerChannel(serverId, confirmUnmap.channel_id);
            setConfirmUnmap(null);
            setUnmapError(null);
            await load(page);
        } catch (e) {
            setUnmapError(messageFrom(e));
        } finally {
            setUnmapping(false);
        }
    };

    if (loading) {
        return <p className='help-text'>{'Loading bridged channels…'}</p>;
    }

    if (error) {
        return (
            <p
                className='help-text'
                style={{color: '#a94442'}}
            >
                {error}
            </p>
        );
    }

    const totalPages = Math.max(1, Math.ceil(totalCount / PER_PAGE));

    return (
        <div style={{padding: '8px 0'}}>
            {mappings.length === 0 ? (
                <p className='help-text'>{'No channels are bridged to this server yet. Use '}<code>{'/matrix map'}</code>{' from inside a channel to bridge it.'}</p>
            ) : (
                <table className='table'>
                    <thead>
                        <tr>
                            <th>{'Channel'}</th>
                            <th>{'Team'}</th>
                            <th>{'Matrix room'}</th>
                            <th>{'Actions'}</th>
                        </tr>
                    </thead>
                    <tbody>
                        {mappings.map((mapping) => (
                            <tr key={mapping.channel_id}>
                                <td>
                                    {mapping.channel_missing ? (
                                        <span style={{color: '#a94442'}}>{'Channel deleted'}</span>
                                    ) : (
                                        mapping.channel_name
                                    )}
                                </td>
                                <td>{mapping.team_name || 'Direct message'}</td>
                                <td style={{fontFamily: 'monospace'}}>{mapping.room_id}</td>
                                <td>
                                    <button
                                        type='button'
                                        className='btn btn-tertiary btn-sm'
                                        onClick={() => {
                                            setUnmapError(null);
                                            setConfirmUnmap(mapping);
                                        }}
                                    >
                                        {'Unmap'}
                                    </button>
                                </td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            )}

            {totalCount > PER_PAGE && (
                <div style={{display: 'flex', gap: '8px', alignItems: 'center'}}>
                    <button
                        type='button'
                        className='btn btn-tertiary btn-sm'
                        disabled={page === 0}
                        onClick={() => load(page - 1)}
                    >
                        {'Previous'}
                    </button>
                    <span>{`Page ${page + 1} of ${totalPages}`}</span>
                    <button
                        type='button'
                        className='btn btn-tertiary btn-sm'
                        disabled={page + 1 >= totalPages}
                        onClick={() => load(page + 1)}
                    >
                        {'Next'}
                    </button>
                </div>
            )}

            {confirmUnmap && (
                <ModalShell
                    title='Unmap channel'
                    onClose={() => setConfirmUnmap(null)}
                    footer={
                        <>
                            <button
                                type='button'
                                className='btn btn-tertiary'
                                disabled={unmapping}
                                onClick={() => setConfirmUnmap(null)}
                            >
                                {'Cancel'}
                            </button>
                            <button
                                type='button'
                                className='btn btn-danger'
                                disabled={unmapping}
                                onClick={handleUnmap}
                            >
                                {unmapping ? 'Unmapping…' : 'Unmap'}
                            </button>
                        </>
                    }
                >
                    <p>
                        {'Unmap '}
                        <strong>{confirmUnmap.channel_missing ? 'this deleted channel' : confirmUnmap.channel_name}</strong>
                        {' from Matrix room '}
                        <code>{confirmUnmap.room_id}</code>
                        {'?'}
                    </p>
                    {unmapError && <p style={{color: '#a94442'}}>{unmapError}</p>}
                </ModalShell>
            )}
        </div>
    );
};

export default MappingsPanel;
