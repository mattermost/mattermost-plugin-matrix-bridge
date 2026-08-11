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
    onClose: () => void;
    onAdded: () => void | Promise<void>;
    onViewRegistration: (server: ServerView) => void;
}

// AddServerModal posts AddServerRequest, omitting every optional field left blank -
// the API mints a server_id and discovers server_name when they're absent, which
// is the point of leaving them blank rather than sending "".
const AddServerModal: React.FC<Props> = ({onClose, onAdded, onViewRegistration}) => {
    const [serverURL, setServerURL] = useState('');
    const [asToken, setASToken] = useState('');
    const [hsToken, setHSToken] = useState('');
    const [usernamePrefix, setUsernamePrefix] = useState('');
    const [showAdvanced, setShowAdvanced] = useState(false);
    const [serverID, setServerID] = useState('');
    const [serverNameOverride, setServerNameOverride] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [created, setCreated] = useState<ServerView | null>(null);

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        setSubmitting(true);
        setError(null);
        try {
            const resp = await client.addServer({
                server_url: serverURL,
                as_token: asToken,
                hs_token: hsToken,
                username_prefix: usernamePrefix || undefined,
                server_id: serverID || undefined,
                server_name: serverNameOverride || undefined,
            });
            setCreated(resp.server);
            await onAdded();
        } catch (err) {
            setError(messageFrom(err));
        } finally {
            setSubmitting(false);
        }
    };

    if (created) {
        return (
            <ModalShell
                title='Matrix server added'
                onClose={onClose}
                footer={
                    <>
                        <button
                            type='button'
                            className='btn btn-tertiary'
                            onClick={onClose}
                        >
                            {'Done'}
                        </button>
                        <button
                            type='button'
                            className='btn btn-primary'
                            onClick={() => onViewRegistration(created)}
                        >
                            {'View registration YAML'}
                        </button>
                    </>
                }
            >
                <p>
                    {'Registered as '}
                    <strong>{created.server_name}</strong>
                    {' ('}
                    <code>{created.server_id}</code>
                    {').'}
                </p>
                <p className='help-text'>
                    {'This server will not start syncing until its Application Service registration is installed on the homeserver.'}
                </p>
            </ModalShell>
        );
    }

    return (
        <ModalShell
            title='Add Matrix server'
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
                        type='submit'
                        form='add-matrix-server-form'
                        className='btn btn-primary'
                        disabled={submitting}
                    >
                        {submitting ? 'Adding…' : 'Add server'}
                    </button>
                </>
            }
        >
            <form
                id='add-matrix-server-form'
                onSubmit={handleSubmit}
            >
                <div className='form-group'>
                    <label htmlFor='matrix-add-server-url'>{'Homeserver URL'}</label>
                    <input
                        id='matrix-add-server-url'
                        className='form-control'
                        type='text'
                        required={true}
                        value={serverURL}
                        placeholder='https://matrix.example.com'
                        onChange={(e) => setServerURL(e.target.value)}
                    />
                </div>
                <div className='form-group'>
                    <label htmlFor='matrix-add-as-token'>{'Application Service token'}</label>
                    <input
                        id='matrix-add-as-token'
                        className='form-control'
                        type='text'
                        required={true}
                        value={asToken}
                        onChange={(e) => setASToken(e.target.value)}
                    />
                </div>
                <div className='form-group'>
                    <label htmlFor='matrix-add-hs-token'>{'Homeserver token'}</label>
                    <input
                        id='matrix-add-hs-token'
                        className='form-control'
                        type='text'
                        required={true}
                        value={hsToken}
                        onChange={(e) => setHSToken(e.target.value)}
                    />
                </div>
                <div className='form-group'>
                    <label htmlFor='matrix-add-username-prefix'>{'Username prefix (optional)'}</label>
                    <input
                        id='matrix-add-username-prefix'
                        className='form-control'
                        type='text'
                        value={usernamePrefix}
                        placeholder='matrix'
                        onChange={(e) => setUsernamePrefix(e.target.value)}
                    />
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
                            <label htmlFor='matrix-add-server-id'>{'Server ID (optional)'}</label>
                            <input
                                id='matrix-add-server-id'
                                className='form-control'
                                type='text'
                                value={serverID}
                                onChange={(e) => setServerID(e.target.value)}
                            />
                            <p className='help-text'>{'Restore a previously removed server by supplying its exact server_id. Leave blank to register a new server.'}</p>
                        </div>
                        <div className='form-group'>
                            <label htmlFor='matrix-add-server-name'>{'Server name override (optional)'}</label>
                            <input
                                id='matrix-add-server-name'
                                className='form-control'
                                type='text'
                                value={serverNameOverride}
                                onChange={(e) => setServerNameOverride(e.target.value)}
                            />
                            <p className='help-text'>{'The Matrix ID domain is discovered automatically from the homeserver. Override this only when the public URL and the Matrix ID domain genuinely differ.'}</p>
                        </div>
                    </div>
                )}

                {error && <p style={{color: '#a94442'}}>{error}</p>}
            </form>
        </ModalShell>
    );
};

export default AddServerModal;
