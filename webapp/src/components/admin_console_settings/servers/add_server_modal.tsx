// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useState} from 'react';

import ModalShell from './modal_shell';
import {formGroupStyle} from './styles';

import * as client from '@/client';
import type {ServerView} from '@/types/matrix';
import {generateToken} from '@/utils/generate_token';

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

    // Not a form onSubmit handler: see the note above the <div onKeyDown={...}>
    // below for why this section can't use a real <form> element.
    const handleSubmit = async () => {
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
                        type='button'
                        className='btn btn-primary'
                        disabled={submitting}
                        onClick={handleSubmit}
                    >
                        {submitting ? 'Adding…' : 'Add server'}
                    </button>
                </>
            }
        >
            {/*
              Deliberately a <div>, not a <form>: ModalShell renders inline rather than
              through a portal, and the System Console's own settings page already
              wraps everything in a Bootstrap <form className="form-horizontal"> (see
              styles.ts). A <form> here would be nested inside that one - invalid HTML
              whose submit-target resolution is browser-dependent - and in practice a
              submit button tied to this form via the `form` attribute can resolve to
              the OUTER form instead, which has no onSubmit of its own and so falls
              through to the browser's native default: a full-page GET reload, with
              this handler never running at all. The Enter key is wired up by hand
              below to keep the one native <form> behaviour worth keeping.
            */}
            <div
                onKeyDown={(e) => {
                    if (e.key === 'Enter' && e.target instanceof HTMLInputElement) {
                        e.preventDefault();

                        // Enter is a submit path of its own, so it needs the guard the footer
                        // buttons get from `disabled`: held down, it would otherwise send a
                        // second write request against the same server.
                        if (!submitting) {
                            handleSubmit();
                        }
                    }
                }}
            >
                <div
                    className='form-group'
                    style={formGroupStyle}
                >
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
                <div
                    className='form-group'
                    style={formGroupStyle}
                >
                    <label htmlFor='matrix-add-as-token'>{'Application Service token'}</label>
                    <div style={{display: 'flex', gap: '6px'}}>
                        <input
                            id='matrix-add-as-token'
                            className='form-control'
                            type='password'
                            autoComplete='off'
                            required={true}
                            style={{flex: 1}}
                            value={asToken}
                            onChange={(e) => setASToken(e.target.value)}
                        />
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            onClick={() => setASToken(generateToken())}
                        >
                            {'Regenerate'}
                        </button>
                    </div>
                </div>
                <div
                    className='form-group'
                    style={formGroupStyle}
                >
                    <label htmlFor='matrix-add-hs-token'>{'Homeserver token'}</label>
                    <div style={{display: 'flex', gap: '6px'}}>
                        <input
                            id='matrix-add-hs-token'
                            className='form-control'
                            type='password'
                            autoComplete='off'
                            required={true}
                            style={{flex: 1}}
                            value={hsToken}
                            onChange={(e) => setHSToken(e.target.value)}
                        />
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            onClick={() => setHSToken(generateToken())}
                        >
                            {'Regenerate'}
                        </button>
                    </div>
                </div>
                <div
                    className='form-group'
                    style={formGroupStyle}
                >
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
                        <div
                            className='form-group'
                            style={formGroupStyle}
                        >
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
                        <div
                            className='form-group'
                            style={formGroupStyle}
                        >
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
            </div>
        </ModalShell>
    );
};

export default AddServerModal;
