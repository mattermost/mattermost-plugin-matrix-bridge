// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useState} from 'react';

import ModalShell from './modal_shell';

import * as client from '@/client';
import type {ServerView} from '@/types/matrix';

function messageFrom(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
}

interface Props {
    server: ServerView;
    onClose: () => void;
    onRemoved: () => void | Promise<void>;
    onDisableInstead: () => void;
}

// RemoveServerDialog surfaces the server_id as the recovery key and the exact
// restore command, since Service.Remove keeps every KV record and an admin who
// loses the ID loses the cheap path back. is_migrated servers can't be removed at
// all - the dialog explains why and offers Disable instead.
const RemoveServerDialog: React.FC<Props> = ({server, onClose, onRemoved, onDisableInstead}) => {
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState<string | null>(null);

    // Tracked separately from recoveryCommand's string value: an empty/falsy
    // recovery_command in the response would otherwise read as "not removed yet"
    // and fall through to the wrong view below.
    const [removed, setRemoved] = useState(false);
    const [recoveryCommand, setRecoveryCommand] = useState<string | null>(null);

    const restoreCommand = `/matrix server add <server_url> <as_token> <hs_token> --server-id ${server.server_id}`;

    const handleRemove = async () => {
        setSubmitting(true);
        setError(null);
        try {
            const resp = await client.removeServer(server.server_id);
            setRecoveryCommand(resp.recovery_command);
            setRemoved(true);
            await onRemoved();
        } catch (err) {
            setError(messageFrom(err));
        } finally {
            setSubmitting(false);
        }
    };

    if (removed) {
        return (
            <ModalShell
                title='Server removed'
                onClose={onClose}
                footer={
                    <button
                        type='button'
                        className='btn btn-primary'
                        onClick={onClose}
                    >
                        {'Done'}
                    </button>
                }
            >
                <p>{'Channel mappings and ghost users for this server were kept, not deleted. To restore it:'}</p>
                <pre>{recoveryCommand}</pre>
            </ModalShell>
        );
    }

    if (server.is_migrated) {
        return (
            <ModalShell
                title='Cannot remove this server'
                onClose={onClose}
                footer={
                    <>
                        <button
                            type='button'
                            className='btn btn-tertiary'
                            onClick={onClose}
                        >
                            {'Cancel'}
                        </button>
                        <button
                            type='button'
                            className='btn btn-primary'
                            onClick={onDisableInstead}
                        >
                            {'Disable instead'}
                        </button>
                    </>
                }
            >
                <p>{'This server was migrated from the legacy single-server configuration and cannot be removed - it has no shared-channels remote identity that could be re-created.'}</p>
                <p>{'Disable it instead to take it out of service without touching its mappings, ghosts or remote.'}</p>
            </ModalShell>
        );
    }

    return (
        <ModalShell
            title={`Remove ${server.server_name}?`}
            onClose={onClose}
            footer={
                <>
                    <button
                        type='button'
                        className='btn btn-tertiary'
                        disabled={submitting}
                        onClick={onClose}
                    >
                        {'Cancel'}
                    </button>
                    <button
                        type='button'
                        className='btn btn-danger'
                        disabled={submitting}
                        onClick={handleRemove}
                    >
                        {submitting ? 'Removing…' : 'Remove'}
                    </button>
                </>
            }
        >
            <p>{'This server\'s channel mappings and ghost users are kept, not deleted, and can be restored with the same server_id:'}</p>
            <p>
                <strong>{'Recovery key (server_id): '}</strong>
                <code>{server.server_id}</code>
            </p>
            <p>{'Restore command:'}</p>
            <pre>{restoreCommand}</pre>
            {error && <p style={{color: '#a94442'}}>{error}</p>}
        </ModalShell>
    );
};

export default RemoveServerDialog;
