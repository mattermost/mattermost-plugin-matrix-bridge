// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useState} from 'react';

import AddServerModal from './add_server_modal';
import EditServerModal from './edit_server_modal';
import RegistrationModal from './registration_modal';
import RemoveServerDialog from './remove_server_dialog';
import ServerTable from './server_table';
import {headerActionsStyle, headerStyle, panelStyle, subtitleStyle, titleStyle} from './styles';
import TestResultsModal from './test_results_modal';
import useServers from './use_servers';

import * as client from '@/client';
import type {ServerView} from '@/types/matrix';

// The console renders a registered custom section with
// {settingsList, sectionTitle, sectionDescription} (schema_admin_settings.tsx).
// settingsList is the rendered list of this section's declared settings - empty
// here, since plugin.json declares none for "matrix_servers" - so these props are
// accepted but unused.
interface Props {
    settingsList?: React.ReactNode;
    sectionTitle?: React.ReactNode;
    sectionDescription?: React.ReactNode;
}

type ModalState =
    | {type: 'none'}
    | {type: 'add'}
    | {type: 'edit'; server: ServerView}
    | {type: 'remove'; server: ServerView}
    | {type: 'test'; server: ServerView}
    | {type: 'registration'; server: ServerView};

// eslint-disable-next-line @typescript-eslint/no-unused-vars
const MatrixServersSection: React.FC<Props> = (_props) => {
    const {servers, countsUnavailable, health, loading, error, refresh, refreshHealth} = useServers();
    const [modal, setModal] = useState<ModalState>({type: 'none'});
    const [expandedServerId, setExpandedServerId] = useState<string | null>(null);
    const [actionError, setActionError] = useState<string | null>(null);

    const closeModal = () => setModal({type: 'none'});

    const handleRefresh = () => {
        refresh();
        refreshHealth();
    };

    const handleToggleEnabled = async (server: ServerView, enabled: boolean) => {
        try {
            await client.setServerEnabled(server.server_id, enabled);
            setActionError(null);
            await refresh();
        } catch (e) {
            setActionError(e instanceof Error ? e.message : String(e));
            throw e;
        }
    };

    return (
        <div>
            <p className='help-text'>
                {'Changes in this section apply immediately over the network and do not require pressing Save at the bottom of the page.'}
            </p>

            {error && <p style={{color: '#a94442'}}>{error}</p>}
            {actionError && <p style={{color: '#a94442'}}>{actionError}</p>}

            <div style={panelStyle}>
                <div style={headerStyle}>
                    <div>
                        <h3 style={titleStyle}>{'Connected Matrix Servers'}</h3>
                        <p style={subtitleStyle}>{'Matrix servers that are connected to this Mattermost server'}</p>
                    </div>
                    <div style={headerActionsStyle}>
                        <button
                            type='button'
                            className='btn btn-tertiary'
                            title='Refresh the list and health status'
                            onClick={handleRefresh}
                        >
                            <i className='icon icon-refresh'/>
                            {'Refresh'}
                        </button>
                        <button
                            type='button'
                            className='btn btn-primary'
                            onClick={() => setModal({type: 'add'})}
                        >
                            <i className='icon icon-plus'/>
                            {'Add a connection'}
                        </button>
                    </div>
                </div>

                <ServerTable
                    servers={servers}
                    countsUnavailable={countsUnavailable}
                    health={health}
                    loading={loading}
                    expandedServerId={expandedServerId}
                    onToggleExpand={(id) => setExpandedServerId(expandedServerId === id ? null : id)}
                    onToggleEnabled={handleToggleEnabled}
                    onEdit={(server) => setModal({type: 'edit', server})}
                    onRemove={(server) => setModal({type: 'remove', server})}
                    onTest={(server) => setModal({type: 'test', server})}
                    onRegistration={(server) => setModal({type: 'registration', server})}
                />
            </div>

            {modal.type === 'add' && (
                <AddServerModal
                    onClose={closeModal}
                    onAdded={refresh}
                    onViewRegistration={(server) => setModal({type: 'registration', server})}
                />
            )}
            {modal.type === 'edit' && (
                <EditServerModal
                    server={modal.server}
                    onClose={closeModal}
                    onUpdated={refresh}
                />
            )}
            {modal.type === 'remove' && (() => {
                const removingServer = modal.server;
                return (
                    <RemoveServerDialog
                        server={removingServer}
                        onClose={closeModal}
                        onRemoved={refresh}
                        onDisableInstead={() => {
                            closeModal();
                            handleToggleEnabled(removingServer, false).catch(() => {
                                // handleToggleEnabled already surfaced the error via actionError.
                            });
                        }}
                    />
                );
            })()}
            {modal.type === 'test' && (
                <TestResultsModal
                    server={modal.server}
                    onClose={closeModal}
                />
            )}
            {modal.type === 'registration' && (
                <RegistrationModal
                    server={modal.server}
                    onClose={closeModal}
                />
            )}
        </div>
    );
};

export default MatrixServersSection;
