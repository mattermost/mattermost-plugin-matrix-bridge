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
    onUpdated: () => void | Promise<void>;
}

// EditServerModal sends a PATCH carrying only what actually changed. Token inputs
// render empty and are omitted from the request unless the admin types a new
// value - sending "" would clear a token and the API rejects it anyway.
const EditServerModal: React.FC<Props> = ({server, onClose, onUpdated}) => {
    const [serverURL, setServerURL] = useState(server.server_url);
    const [asToken, setASToken] = useState('');
    const [hsToken, setHSToken] = useState('');
    const [usernamePrefix, setUsernamePrefix] = useState(server.username_prefix);
    const [showAdvanced, setShowAdvanced] = useState(false);
    const [serverName, setServerName] = useState(server.server_name);
    const [confirmNameChange, setConfirmNameChange] = useState(false);
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [warnings, setWarnings] = useState<string[]>([]);
    const [saved, setSaved] = useState(false);

    const serverNameChanged = serverName.trim() !== server.server_name;

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        if (serverNameChanged && !confirmNameChange) {
            setError('Check the confirmation box below before saving a server name change.');
            return;
        }
        setSubmitting(true);
        setError(null);
        try {
            const resp = await client.updateServer(server.server_id, {
                server_url: serverURL === server.server_url ? undefined : serverURL,
                as_token: asToken || undefined,
                hs_token: hsToken || undefined,
                username_prefix: usernamePrefix === server.username_prefix ? undefined : usernamePrefix,
                server_name: serverNameChanged ? serverName : undefined,
            });
            setWarnings(resp.warnings);
            setSaved(true);
            await onUpdated();
        } catch (err) {
            setError(messageFrom(err));
        } finally {
            setSubmitting(false);
        }
    };

    return (
        <ModalShell
            title={`Edit ${server.server_name}`}
            onClose={onClose}
            footer={
                saved ? (
                    <button
                        type='button'
                        className='btn btn-primary'
                        onClick={onClose}
                    >
                        {'Done'}
                    </button>
                ) : (
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
                            type='submit'
                            form='edit-matrix-server-form'
                            className='btn btn-primary'
                            disabled={submitting}
                        >
                            {submitting ? 'Saving…' : 'Save'}
                        </button>
                    </>
                )
            }
        >
            {saved ? (
                <div>
                    <p>{'Server updated.'}</p>
                    {warnings.map((w) => (
                        <p
                            key={w}
                            style={{color: '#a06400'}}
                        >
                            {w}
                        </p>
                    ))}
                </div>
            ) : (
                <form
                    id='edit-matrix-server-form'
                    onSubmit={handleSubmit}
                >
                    <div className='form-group'>
                        <label htmlFor='matrix-edit-server-url'>{'Homeserver URL'}</label>
                        <input
                            id='matrix-edit-server-url'
                            className='form-control'
                            type='text'
                            required={true}
                            value={serverURL}
                            onChange={(e) => setServerURL(e.target.value)}
                        />
                    </div>
                    <div className='form-group'>
                        <label htmlFor='matrix-edit-as-token'>{'Application Service token'}</label>
                        <input
                            id='matrix-edit-as-token'
                            className='form-control'
                            type='text'
                            placeholder={server.has_as_token ? 'configured — leave blank to keep' : 'not configured'}
                            value={asToken}
                            onChange={(e) => setASToken(e.target.value)}
                        />
                    </div>
                    <div className='form-group'>
                        <label htmlFor='matrix-edit-hs-token'>{'Homeserver token'}</label>
                        <input
                            id='matrix-edit-hs-token'
                            className='form-control'
                            type='text'
                            placeholder={server.has_hs_token ? 'configured — leave blank to keep' : 'not configured'}
                            value={hsToken}
                            onChange={(e) => setHSToken(e.target.value)}
                        />
                    </div>
                    <div className='form-group'>
                        <label htmlFor='matrix-edit-username-prefix'>{'Username prefix'}</label>
                        <input
                            id='matrix-edit-username-prefix'
                            className='form-control'
                            type='text'
                            value={usernamePrefix}
                            onChange={(e) => setUsernamePrefix(e.target.value)}
                        />
                        <p className='help-text'>{'Only names new Mattermost users created for Matrix-originated users going forward; existing users keep their usernames.'}</p>
                    </div>

                    <button
                        type='button'
                        className='btn btn-tertiary btn-sm'
                        onClick={() => setShowAdvanced(!showAdvanced)}
                    >
                        {showAdvanced ? 'Hide advanced' : 'Show advanced'}
                    </button>

                    {showAdvanced && (
                        <div style={{marginTop: '12px'}}>
                            <div className='form-group'>
                                <label htmlFor='matrix-edit-server-name'>{'Server name'}</label>
                                <input
                                    id='matrix-edit-server-name'
                                    className='form-control'
                                    type='text'
                                    value={serverName}
                                    onChange={(e) => setServerName(e.target.value)}
                                />
                            </div>
                            {serverNameChanged && (
                                <div className='form-group has-warning'>
                                    <label
                                        htmlFor='matrix-edit-confirm-name-change'
                                        style={{display: 'flex', gap: '6px', alignItems: 'flex-start'}}
                                    >
                                        <input
                                            id='matrix-edit-confirm-name-change'
                                            type='checkbox'
                                            checked={confirmNameChange}
                                            onChange={(e) => setConfirmNameChange(e.target.checked)}
                                        />
                                        <span>{'I understand changing the server name makes every existing ghost user for this server unrecognized as one, and inbound events from them will be treated as regular Matrix users\' events until they are recreated.'}</span>
                                    </label>
                                </div>
                            )}
                        </div>
                    )}

                    {error && <p style={{color: '#a94442'}}>{error}</p>}
                </form>
            )}
        </ModalShell>
    );
};

export default EditServerModal;
