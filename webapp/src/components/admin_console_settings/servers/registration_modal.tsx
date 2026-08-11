// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useEffect, useState} from 'react';

import ModalShell from './modal_shell';

import * as client from '@/client';
import type {RegistrationResponse, ServerView} from '@/types/matrix';

function messageFrom(e: unknown): string {
    return e instanceof Error ? e.message : String(e);
}

interface Props {
    server: ServerView;
    onClose: () => void;
}

const codeBlockStyle: React.CSSProperties = {
    backgroundColor: '#f4f4f4',
    border: '1px solid #ddd',
    borderRadius: '4px',
    padding: '12px',
    fontFamily: 'Monaco, Menlo, "Ubuntu Mono", monospace',
    fontSize: '12px',
    whiteSpace: 'pre',
    overflow: 'auto',
    margin: '8px 0',
};

function copyToClipboard(text: string) {
    navigator.clipboard?.writeText(text).catch(() => {
        // Best-effort only - the content is still visible and selectable by hand.
    });
}

// RegistrationModal shows the Application Service registration YAML verbatim
// (backend §3.9: copy it as-is, never append /_matrix/app/v1), a download using the
// response's own filename, and the room_list_publication_rules snippet - the one
// piece of real content the deleted homeserver_config component carried, now
// filled in from server_name instead of scraped out of a DOM input.
const RegistrationModal: React.FC<Props> = ({server, onClose}) => {
    const [registration, setRegistration] = useState<RegistrationResponse | null>(null);
    const [error, setError] = useState<string | null>(null);

    useEffect(() => {
        client.getServerRegistration(server.server_id).then(setRegistration).catch((e) => setError(messageFrom(e)));
    }, [server.server_id]);

    const download = () => {
        if (!registration) {
            return;
        }
        const blob = new Blob([registration.content], {type: 'text/yaml'});
        const url = URL.createObjectURL(blob);
        const link = document.createElement('a');
        link.href = url;
        link.download = registration.filename;
        document.body.appendChild(link);
        link.click();
        document.body.removeChild(link);
        URL.revokeObjectURL(url);
    };

    const roomListRules = `room_list_publication_rules:
  - user_id: "@_mattermost_bridge:${server.server_name}"
    action: allow
  - user_id: "*"
    action: deny`;

    return (
        <ModalShell
            title={`Application Service registration for ${server.server_name}`}
            onClose={onClose}
            footer={
                <button
                    type='button'
                    className='btn btn-primary'
                    onClick={onClose}
                >
                    {'Close'}
                </button>
            }
        >
            {error && <p style={{color: '#a94442'}}>{error}</p>}
            {registration && (
                <>
                    <p style={{color: '#a06400'}}>
                        {'Copy this file verbatim onto your homeserver. Do not append '}
                        <code>{'/_matrix/app/v1'}</code>
                        {' to its url line - the homeserver appends that path itself, and appending it here breaks all inbound sync for this server.'}
                    </p>
                    <div style={{display: 'flex', gap: '8px', marginBottom: '8px'}}>
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            onClick={() => copyToClipboard(registration.content)}
                        >
                            {'Copy'}
                        </button>
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            onClick={download}
                        >
                            {'Download'}
                        </button>
                    </div>
                    <div style={codeBlockStyle}>{registration.content}</div>

                    <p style={{marginTop: '16px'}}>
                        {'To restrict room-directory publication to this bridge, add this to your homeserver.yaml:'}
                    </p>
                    <div style={{display: 'flex', gap: '8px', marginBottom: '8px'}}>
                        <button
                            type='button'
                            className='btn btn-tertiary btn-sm'
                            onClick={() => copyToClipboard(roomListRules)}
                        >
                            {'Copy'}
                        </button>
                    </div>
                    <div style={codeBlockStyle}>{roomListRules}</div>
                </>
            )}
        </ModalShell>
    );
};

export default RegistrationModal;
