// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useEffect, useRef, useState} from 'react';

import MappingsPanel from './mappings_panel';
import {
    cellStyle,
    iconButtonStyle,
    menuAnchorStyle,
    menuItemStyle,
    menuStyle,
    metaStyle,
    migratedTagStyle,
    mappingsWrapperStyle,
    mutedCellTextStyle,
    nameStyle,
    pillDotStyle,
    pillStyle,
    rowStyle,
    serverIdStyle,
} from './styles';
import type {PillVariant} from './styles';

import type {ServerView} from '@/types/matrix';

interface Props {
    server: ServerView;
    health?: string;
    expanded: boolean;
    onToggleExpand: () => void;
    onToggleEnabled: (server: ServerView, enabled: boolean) => Promise<void>;
    onEdit: (server: ServerView) => void;
    onRemove: (server: ServerView) => void;
    onTest: (server: ServerView) => void;
    onRegistration: (server: ServerView) => void;
}

function copyToClipboard(text: string) {
    navigator.clipboard?.writeText(text).catch(() => {
        // Best-effort only - the ID is still visible and selectable by hand.
    });
}

interface Pill {
    label: string;
    variant: PillVariant;
    title?: string;
}

// A single status pill replaces the old separate State/Health columns. Health
// is probed separately from the list (GET /servers/health) and may not have
// arrived yet, so an undefined health falls back to the enabled flag rather
// than rendering as an error. `enabled` comes fresh with every GET /servers,
// while `health` is a separate, not-auto-refreshed fetch - so it can still say
// "healthy" for a server that was just disabled. Check `enabled` first so a
// stale healthy reading never overrides a server that's actually off.
function pillFor(enabled: boolean, health?: string): Pill {
    if (health === 'disabled' || !enabled) {
        return {label: 'Disabled', variant: 'disabled'};
    }
    if (health === 'healthy') {
        return {label: 'Active', variant: 'active'};
    }
    if (health === 'unavailable' || health === 'unhealthy' || health === 'timed out') {
        return {label: 'Unhealthy', variant: 'error', title: `Health check reported: ${health}`};
    }
    return {label: 'Active', variant: 'active'};
}

function channelsSharedText(count: number | null): string {
    if (count === null) {
        return 'unavailable';
    }
    return `${count} channel${count === 1 ? '' : 's'} shared`;
}

const ServerRow: React.FC<Props> = ({server, health, expanded, onToggleExpand, onToggleEnabled, onEdit, onRemove, onTest, onRegistration}) => {
    const [enabling, setEnabling] = useState(false);
    const [optimisticEnabled, setOptimisticEnabled] = useState<boolean | null>(null);
    const [menuOpen, setMenuOpen] = useState(false);
    const [menuButtonHovered, setMenuButtonHovered] = useState(false);
    const menuAnchorRef = useRef<HTMLDivElement>(null);

    const enabled = optimisticEnabled === null ? server.enabled : optimisticEnabled;

    useEffect(() => {
        if (!menuOpen) {
            return undefined;
        }

        const handleClickOutside = (event: MouseEvent) => {
            if (menuAnchorRef.current && !menuAnchorRef.current.contains(event.target as Node)) {
                setMenuOpen(false);
            }
        };
        const handleEscape = (event: KeyboardEvent) => {
            if (event.key === 'Escape') {
                setMenuOpen(false);
            }
        };

        document.addEventListener('mousedown', handleClickOutside);
        document.addEventListener('keydown', handleEscape);
        return () => {
            document.removeEventListener('mousedown', handleClickOutside);
            document.removeEventListener('keydown', handleEscape);
        };
    }, [menuOpen]);

    const handleToggle = async () => {
        const next = !enabled;
        setOptimisticEnabled(next);
        setEnabling(true);
        try {
            await onToggleEnabled(server, next);

            // Success - defer back to server.enabled (via the refreshed prop) instead
            // of staying pinned to the optimistic value, so a later external change
            // (e.g. another admin toggling it) is reflected instead of masked.
            setOptimisticEnabled(null);
        } catch (e) {
            // Roll back the optimistic flip - the mutation failed server-side.
            setOptimisticEnabled(!next);
        } finally {
            setEnabling(false);
        }
    };

    const runAndClose = (action: () => void) => {
        setMenuOpen(false);
        action();
    };

    const pill = pillFor(enabled, health);

    return (
        <>
            <div style={rowStyle}>
                <div style={cellStyle}>
                    <div style={nameStyle}>{server.server_name}</div>
                    <button
                        type='button'
                        className='style--none'
                        style={serverIdStyle}
                        title='Copy server ID'
                        onClick={() => copyToClipboard(server.server_id)}
                    >
                        {server.server_id}
                    </button>
                    {server.is_migrated && (
                        <div style={migratedTagStyle}>{'Migrated from legacy configuration'}</div>
                    )}
                </div>
                <div style={{...cellStyle, ...mutedCellTextStyle}}>{channelsSharedText(server.mapped_channel_count)}</div>
                <div style={{...cellStyle, ...mutedCellTextStyle}}>{server.server_url}</div>
                <div style={metaStyle}>
                    <span
                        style={pillStyle(pill.variant)}
                        title={pill.title}
                    >
                        <span style={pillDotStyle(pill.variant)}/>
                        {pill.label}
                    </span>
                    <div
                        ref={menuAnchorRef}
                        style={menuAnchorStyle}
                    >
                        <button
                            type='button'
                            style={iconButtonStyle(menuButtonHovered)}
                            aria-label={`Actions for ${server.server_name}`}
                            aria-expanded={menuOpen}
                            onMouseEnter={() => setMenuButtonHovered(true)}
                            onMouseLeave={() => setMenuButtonHovered(false)}
                            onClick={() => setMenuOpen((open) => !open)}
                        >
                            <i className='icon icon-dots-horizontal'/>
                        </button>
                        {menuOpen && (
                            <div
                                style={menuStyle}
                                role='menu'
                            >
                                <button
                                    type='button'
                                    role='menuitem'
                                    style={menuItemStyle({disabled: enabling})}
                                    disabled={enabling}
                                    onClick={() => runAndClose(handleToggle)}
                                >
                                    <i className='icon icon-power-plug-outline'/>
                                    {enabled ? 'Disable connection' : 'Enable connection'}
                                </button>
                                <button
                                    type='button'
                                    role='menuitem'
                                    style={menuItemStyle({})}
                                    onClick={() => runAndClose(() => onTest(server))}
                                >
                                    <i className='icon icon-flask-outline'/>
                                    {'Test connection'}
                                </button>
                                <button
                                    type='button'
                                    role='menuitem'
                                    style={menuItemStyle({})}
                                    onClick={() => runAndClose(() => onRegistration(server))}
                                >
                                    <i className='icon icon-key-variant'/>
                                    {'View registration'}
                                </button>
                                <button
                                    type='button'
                                    role='menuitem'
                                    style={menuItemStyle({})}
                                    onClick={() => runAndClose(onToggleExpand)}
                                >
                                    <i className='icon icon-format-list-bulleted'/>
                                    {expanded ? 'Hide bridged channels' : 'Show bridged channels'}
                                </button>
                                <button
                                    type='button'
                                    role='menuitem'
                                    style={menuItemStyle({})}
                                    onClick={() => runAndClose(() => onEdit(server))}
                                >
                                    <i className='icon icon-pencil-outline'/>
                                    {'Edit'}
                                </button>
                                <button
                                    type='button'
                                    role='menuitem'
                                    style={menuItemStyle({danger: true, disabled: server.is_migrated})}
                                    disabled={server.is_migrated}
                                    title={server.is_migrated ? 'This server was migrated from the legacy configuration and cannot be removed - use Disable connection instead' : undefined}
                                    onClick={() => runAndClose(() => onRemove(server))}
                                >
                                    <i className='icon icon-trash-can-outline'/>
                                    {'Remove'}
                                </button>
                            </div>
                        )}
                    </div>
                </div>
            </div>
            {expanded && (
                <div style={mappingsWrapperStyle}>
                    <MappingsPanel serverId={server.server_id}/>
                </div>
            )}
        </>
    );
};

export default ServerRow;
