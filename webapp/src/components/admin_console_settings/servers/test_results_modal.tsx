// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useEffect, useState} from 'react';

import ModalShell from './modal_shell';

import * as client from '@/client';
import type {Diagnostics, ServerView} from '@/types/matrix';

function messageFrom(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
}

interface Props {
    server: ServerView;
    onClose: () => void;
}

const STATUS_ICON: Record<string, string> = {
    ok: '✅',
    fail: '❌',
    skip: '⏭️',
};

// TestResultsModal runs Servers().Diagnose on open and renders its checklist,
// rendering "skip" distinctly from "fail" - a skipped check never implies a pass.
// runTest guards against a second click stacking a request while one is already
// in flight.
const TestResultsModal: React.FC<Props> = ({server, onClose}) => {
    const [diagnostics, setDiagnostics] = useState<Diagnostics | null>(null);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState<string | null>(null);

    const runTest = async () => {
        if (loading) {
            return;
        }
        setLoading(true);
        setError(null);
        try {
            const result = await client.testServer(server.server_id);
            setDiagnostics(result);
        } catch (e) {
            setError(messageFrom(e));
        } finally {
            setLoading(false);
        }
    };

    useEffect(() => {
        runTest();

        // Intentionally run once, on open, only.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    return (
        <ModalShell
            title={`Test ${server.server_name}`}
            onClose={onClose}
            footer={
                <>
                    <button
                        type='button'
                        className='btn btn-tertiary'
                        disabled={loading}
                        onClick={runTest}
                    >
                        {loading ? 'Testing…' : 'Run again'}
                    </button>
                    <button
                        type='button'
                        className='btn btn-primary'
                        onClick={onClose}
                    >
                        {'Close'}
                    </button>
                </>
            }
        >
            {loading && <p>{'Running diagnostics…'}</p>}
            {error && <p style={{color: '#a94442'}}>{error}</p>}
            {diagnostics && !loading && (
                <div>
                    <ul style={{listStyle: 'none', paddingLeft: 0}}>
                        {diagnostics.checks.map((check) => (
                            <li
                                key={check.key}
                                style={{marginBottom: '8px'}}
                            >
                                <span>{STATUS_ICON[check.status] || '•'}</span>
                                {' '}
                                <strong>{check.label}</strong>
                                {check.detail && <span>{`: ${check.detail}`}</span>}
                                {check.status === 'skip' && <span style={{color: '#666'}}>{' (skipped)'}</span>}
                            </li>
                        ))}
                    </ul>
                    {diagnostics.server_info && (
                        <p className='help-text'>
                            {`Matrix server: ${diagnostics.server_info.name}`}
                            {diagnostics.server_info.version === 'Unknown' ? '' : ` v${diagnostics.server_info.version}`}
                        </p>
                    )}
                </div>
            )}
        </ModalShell>
    );
};

export default TestResultsModal;
