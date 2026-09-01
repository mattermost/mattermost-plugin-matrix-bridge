// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {useCallback, useEffect, useRef, useState} from 'react';

import * as client from '@/client';
import type {ServerView} from '@/types/matrix';

function messageFrom(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
}

export interface UseServersResult {
    servers: ServerView[];
    health: Record<string, string>;
    loading: boolean;
    error: string | null;

    // refresh re-fetches the list (a fresh KV read server-side) - call after any
    // mutation. refreshHealth re-probes health separately, since probing costs up
    // to several seconds; neither is polled automatically (§3.8: "No auto-polling").
    refresh: () => Promise<void>;
    refreshHealth: () => Promise<void>;
}

export default function useServers(): UseServersResult {
    const [servers, setServers] = useState<ServerView[]>([]);
    const [health, setHealth] = useState<Record<string, string>>({});
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);

    // Guards against out-of-order responses: if refresh() is called again before
    // an in-flight call resolves (e.g. two mutations in quick succession), only the
    // response from the LATEST call is allowed to land in state.
    const refreshRequestID = useRef(0);

    // The same guard for health, tracked separately because the two are fetched
    // independently. A probe round fans out over the network and the server bounds
    // it at 8s, so overlapping rounds are considerably more likely here than for
    // the list - and every mutation now triggers one.
    const healthRequestID = useRef(0);

    const refresh = useCallback(async () => {
        const requestID = ++refreshRequestID.current;
        setLoading(true);
        try {
            const resp = await client.listServers();
            if (requestID !== refreshRequestID.current) {
                return;
            }
            setServers(resp.servers);
            setError(null);
        } catch (e) {
            if (requestID !== refreshRequestID.current) {
                return;
            }
            setError(messageFrom(e));
        } finally {
            if (requestID === refreshRequestID.current) {
                setLoading(false);
            }
        }
    }, []);

    const refreshHealth = useCallback(async () => {
        const requestID = ++healthRequestID.current;
        try {
            const resp = await client.getServersHealth();
            if (requestID !== healthRequestID.current) {
                return;
            }
            setHealth(resp.health);
        } catch (e) {
            // Health is supplementary - a failed probe round leaves the table's
            // existing (or empty) health column alone rather than surfacing a
            // page-level error over a non-essential column.
        }
    }, []);

    useEffect(() => {
        let mounted = true;
        (async () => {
            await refresh();
            if (mounted) {
                await refreshHealth();
            }
        })();
        return () => {
            mounted = false;
        };

        // Only ever run once on mount - refresh/refreshHealth are stable (useCallback
        // with no deps) and every other refresh is triggered explicitly by a
        // mutation or the Refresh button, never automatically.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    return {servers, health, loading, error, refresh, refreshHealth};
}
