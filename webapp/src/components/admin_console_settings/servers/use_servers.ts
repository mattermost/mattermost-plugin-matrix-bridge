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

    // refreshAll is what a mutation wants: the list and the health readings are
    // both stale afterwards, and a row's pill is derived from the two together.
    // refresh and refreshHealth are exposed for the mount sequence, which fetches
    // the list first so rows appear without waiting on a probe round. Nothing is
    // polled automatically (§3.8: "No auto-polling").
    refreshAll: () => Promise<void>;
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
    // it at 8s, so rounds overlap far more readily than list fetches do - every
    // mutation starts one, as does the Refresh button.
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

    // Concurrent, not sequential: the two reads are independent, and a row reads
    // health by server_id rather than by position in the list, so neither result
    // depends on the other having landed. Awaiting them in series would add a
    // probe round (up to 8s) to the time a mutation's controls stay disabled.
    const refreshAll = useCallback(async () => {
        await Promise.all([refresh(), refreshHealth()]);
    }, [refresh, refreshHealth]);

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

    return {servers, health, loading, error, refreshAll, refresh, refreshHealth};
}
