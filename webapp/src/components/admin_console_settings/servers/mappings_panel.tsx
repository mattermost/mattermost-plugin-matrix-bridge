// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useCallback, useEffect, useState} from 'react';

import {
    cellStyle,
    colors,
    mappingsEmptyStyle,
    mappingsHeaderRowStyle,
    mappingsRoomIDStyle,
    mappingsRowStyle,
    mutedCellTextStyle,
} from './styles';

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
//
// Read-only: unmapping a channel is done via `/matrix unmap`/`/matrix server
// unmap` (System Admin only) - there is no System Console action or API for it,
// by design, since it also uninvites the plugin from the shared channel and
// clears Matrix room state, which deserves the slash command's explicit
// confirmation rather than a stray click in a list.
//
// Uses the same row/cell/muted-text styling as the top-level server list
// (styles.ts) rather than a native <table>, so this nested list reads as part of
// the same design instead of a plain browser table dropped into it.
const MappingsPanel: React.FC<Props> = ({serverId}) => {
    const [mappings, setMappings] = useState<MappingView[]>([]);
    const [totalCount, setTotalCount] = useState(0);
    const [page, setPage] = useState(0);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);

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

    if (loading) {
        return (
            <p
                className='help-text'
                style={mappingsEmptyStyle}
            >
                {'Loading bridged channels…'}
            </p>
        );
    }

    if (error) {
        return (
            <p
                className='help-text'
                style={{...mappingsEmptyStyle, color: colors.red}}
            >
                {error}
            </p>
        );
    }

    const totalPages = Math.max(1, Math.ceil(totalCount / PER_PAGE));

    return (
        <div>
            {mappings.length === 0 ? (
                <p
                    className='help-text'
                    style={mappingsEmptyStyle}
                >
                    {'No channels are bridged to this server yet. Use '}<code>{'/matrix map'}</code>{' from inside a channel to bridge it.'}
                </p>
            ) : (
                <div>
                    <div style={mappingsHeaderRowStyle}>
                        <div style={cellStyle}>{'Channel'}</div>
                        <div style={cellStyle}>{'Team'}</div>
                        <div style={cellStyle}>{'Matrix room'}</div>
                    </div>
                    {mappings.map((mapping) => (
                        <div
                            key={mapping.channel_id}
                            style={mappingsRowStyle}
                        >
                            <div style={cellStyle}>
                                {mapping.channel_missing ? (
                                    <span style={{color: colors.red}}>{'Channel deleted'}</span>
                                ) : (
                                    mapping.channel_name
                                )}
                            </div>
                            <div style={{...cellStyle, ...mutedCellTextStyle}}>{mapping.team_name || 'Direct message'}</div>
                            <div style={{...cellStyle, ...mappingsRoomIDStyle}}>{mapping.room_id}</div>
                        </div>
                    ))}
                </div>
            )}

            {totalCount > PER_PAGE && (
                <div style={{display: 'flex', gap: '8px', alignItems: 'center', paddingTop: '12px'}}>
                    <button
                        type='button'
                        className='btn btn-tertiary btn-sm'
                        disabled={page === 0}
                        onClick={() => load(page - 1)}
                    >
                        {'Previous'}
                    </button>
                    <span
                        className='help-text'
                        style={{margin: 0}}
                    >
                        {`Page ${page + 1} of ${totalPages}`}
                    </span>
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
        </div>
    );
};

export default MappingsPanel;
