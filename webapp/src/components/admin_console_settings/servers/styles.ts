// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import type {CSSProperties} from 'react';

// Shared style constants for the connections list (index.tsx, server_table.tsx,
// server_row.tsx), matching the "Connected Matrix Servers" panel from the Figma
// design (node 6056:54388). Kept as plain inline-style objects rather than a
// stylesheet - this plugin's webapp is dependency-free and doesn't pull in a CSS
// file anywhere else (see modal_shell.tsx) - so this stays consistent with that.
//
// Colors mirror Mattermost's Denim tokens (var(--center-channel-color) etc.).
// The "-75"/"-64"/"-16"/"-12"/"-8" opacity variants the design uses are not
// defined as custom properties, so they are composed from
// --center-channel-color-rgb, which Mattermost exposes as a bare "r, g, b"
// triplet for exactly this purpose. The fallback is Denim's #3f4350, used only
// when the property is absent - a hardcoded rgba() here would instead keep
// light-theme text and borders on a dark-theme background.
export const colors = {
    textMuted: 'rgba(var(--center-channel-color-rgb, 63, 67, 80), 0.75)',
    textFaint: 'rgba(var(--center-channel-color-rgb, 63, 67, 80), 0.64)',
    borderFaint: 'rgba(var(--center-channel-color-rgb, 63, 67, 80), 0.08)',
    border: 'rgba(var(--center-channel-color-rgb, 63, 67, 80), 0.12)',
    borderStrong: 'rgba(var(--center-channel-color-rgb, 63, 67, 80), 0.16)',
    hover: 'rgba(var(--center-channel-color-rgb, 63, 67, 80), 0.08)',
    green: '#339970',
    greenBg: 'rgba(51, 153, 112, 0.08)',
    red: 'var(--error-text, #d24b4e)',
    redBg: 'rgba(210, 75, 78, 0.08)',
};

export const panelStyle: CSSProperties = {
    background: 'var(--center-channel-bg, #fff)',
    color: 'var(--center-channel-color, #3f4350)',
    border: `1px solid ${colors.border}`,
    borderRadius: '8px',
    boxShadow: '0 2px 3px rgba(0, 0, 0, 0.08)',
};

export const headerStyle: CSSProperties = {
    display: 'flex',
    alignItems: 'flex-start',
    justifyContent: 'space-between',
    gap: '24px',
    padding: '24px',
    borderBottom: `1px solid ${colors.border}`,
};

export const titleStyle: CSSProperties = {
    margin: 0,
    fontFamily: "Metropolis, 'Open Sans', sans-serif",
    fontWeight: 600,
    fontSize: '16px',
    lineHeight: '24px',
};

export const subtitleStyle: CSSProperties = {
    margin: '4px 0 0',
    fontSize: '14px',
    lineHeight: '20px',
    color: colors.textMuted,
};

export const headerActionsStyle: CSSProperties = {
    display: 'flex',
    gap: '8px',
    alignItems: 'center',
    flexShrink: 0,
};

export const rowStyle: CSSProperties = {
    display: 'flex',
    alignItems: 'center',
    gap: '24px',
    padding: '16px 24px',
    borderBottom: `1px solid ${colors.borderFaint}`,
    position: 'relative',
};

export const cellStyle: CSSProperties = {
    flex: '1 1 0',
    minWidth: 0,
};

export const nameStyle: CSSProperties = {
    fontSize: '14px',
    fontWeight: 600,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
};

export const mutedCellTextStyle: CSSProperties = {
    fontSize: '12px',
    color: colors.textMuted,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
};

export const serverIdStyle: CSSProperties = {
    fontFamily: 'monospace',
    fontSize: '12px',
    color: colors.textMuted,
    background: 'none',
    border: 'none',
    padding: 0,
    cursor: 'pointer',
    display: 'block',
};

export const metaStyle: CSSProperties = {
    display: 'flex',
    alignItems: 'center',
    gap: '16px',
    flexShrink: 0,
};

export type PillVariant = 'active' | 'disabled' | 'error';

const pillPalette: Record<PillVariant, {bg: string; fg: string}> = {
    active: {bg: colors.greenBg, fg: colors.green},
    disabled: {bg: colors.hover, fg: colors.textMuted},
    error: {bg: colors.redBg, fg: colors.red},
};

export function pillStyle(variant: PillVariant): CSSProperties {
    const palette = pillPalette[variant];
    return {
        display: 'inline-flex',
        alignItems: 'center',
        gap: '5px',
        padding: '4px 10px 4px 6px',
        borderRadius: '17px',
        fontSize: '12px',
        fontWeight: 600,
        whiteSpace: 'nowrap',
        background: palette.bg,
        color: palette.fg,
    };
}

export function pillDotStyle(variant: PillVariant): CSSProperties {
    const dotColor: Record<PillVariant, string> = {active: colors.green, disabled: colors.textFaint, error: colors.red};
    return {
        width: '6px',
        height: '6px',
        borderRadius: '50%',
        background: dotColor[variant],
        flexShrink: 0,
    };
}

export function iconButtonStyle(hovered: boolean): CSSProperties {
    return {
        display: 'inline-flex',
        alignItems: 'center',
        justifyContent: 'center',
        width: '28px',
        height: '28px',
        borderRadius: '4px',
        border: 'none',
        background: hovered ? colors.hover : 'transparent',
        color: colors.textFaint,
        cursor: 'pointer',
        flexShrink: 0,
    };
}

export const menuAnchorStyle: CSSProperties = {
    position: 'relative',
    display: 'inline-flex',
};

export const menuStyle: CSSProperties = {
    position: 'absolute',
    top: 'calc(100% + 4px)',
    right: 0,
    minWidth: '232px',
    background: 'var(--center-channel-bg, #fff)',
    border: `1px solid ${colors.borderStrong}`,
    borderRadius: '4px',
    boxShadow: '0 8px 24px rgba(0, 0, 0, 0.12)',
    padding: '8px 0',
    zIndex: 5,
};

function menuItemColor(opts: {danger?: boolean; disabled?: boolean}): string {
    if (opts.disabled) {
        return colors.textFaint;
    }
    return opts.danger ? colors.red : 'inherit';
}

export function menuItemStyle(opts: {danger?: boolean; disabled?: boolean}): CSSProperties {
    return {
        display: 'flex',
        alignItems: 'center',
        gap: '8px',
        width: '100%',
        padding: '6px 20px 6px 16px',
        background: 'none',
        border: 'none',
        textAlign: 'left',
        fontSize: '14px',
        color: menuItemColor(opts),
        cursor: opts.disabled ? 'not-allowed' : 'pointer',
    };
}

export const emptyStateStyle: CSSProperties = {
    padding: '40px 24px',
};

// The admin console's own settings page wraps everything in a Bootstrap
// <form className="form-horizontal">, and its `.form-horizontal .form-group`
// rule (margin: 0 -15px, meant to offset .col-sm-* grid columns) reaches into
// this section's modals too since they render inline rather than through a
// portal. That leaves modal form fields 15px narrower than the modal's own
// 24px body padding on each side - cancel it out explicitly rather than
// dropping the "form-group" class, which also supplies the bold label spacing
// used throughout the rest of the admin console.
export const formGroupStyle: CSSProperties = {
    marginLeft: 0,
    marginRight: 0,
};

export const mappingsWrapperStyle: CSSProperties = {
    padding: '16px 24px',
    borderBottom: `1px solid ${colors.borderFaint}`,
};

// The mappings list nested inside an expanded server row - same row/cell/muted-
// text language as the top-level server list (rowStyle/cellStyle/mutedCellTextStyle
// above), just lighter: it's a sub-list, not its own panel, so no border/background
// of its own and less vertical padding per row.
export const mappingsHeaderRowStyle: CSSProperties = {
    display: 'flex',
    gap: '24px',
    padding: '0 0 8px',
    borderBottom: `1px solid ${colors.borderFaint}`,
    fontSize: '12px',
    fontWeight: 600,
    color: colors.textFaint,
    textTransform: 'uppercase',
    letterSpacing: '0.02em',
};

export const mappingsRowStyle: CSSProperties = {
    display: 'flex',
    alignItems: 'center',
    gap: '24px',
    padding: '10px 0',
    borderBottom: `1px solid ${colors.borderFaint}`,
};

export const mappingsRoomIDStyle: CSSProperties = {
    fontFamily: 'monospace',
    fontSize: '12px',
    color: colors.textMuted,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
};

export const mappingsEmptyStyle: CSSProperties = {
    padding: '16px 0',
};
