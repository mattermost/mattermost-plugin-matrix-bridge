// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React, {useEffect, useRef} from 'react';

interface Props {
    title: string;
    onClose: () => void;
    children: React.ReactNode;
    footer?: React.ReactNode;
}

// ModalShell is a minimal, dependency-free modal frame shared by every dialog in
// this section - this plugin's webapp does not pull in a design-system modal
// component elsewhere, so this stays consistent with that.
const overlayStyle: React.CSSProperties = {
    position: 'fixed',
    top: 0,
    left: 0,
    right: 0,
    bottom: 0,
    backgroundColor: 'rgba(0, 0, 0, 0.5)',
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    zIndex: 1050,
};

const dialogStyle: React.CSSProperties = {
    backgroundColor: 'var(--center-channel-bg)',
    color: 'var(--center-channel-color)',
    borderRadius: '4px',
    minWidth: '420px',
    maxWidth: '640px',
    maxHeight: '85vh',
    overflowY: 'auto',
    boxShadow: '0 4px 24px rgba(0, 0, 0, 0.3)',
};

const headerStyle: React.CSSProperties = {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'space-between',
    padding: '20px 24px',
    borderBottom: '1px solid rgba(var(--center-channel-color-rgb), 0.16)',
};

const bodyStyle: React.CSSProperties = {
    padding: '24px',
};

const footerStyle: React.CSSProperties = {
    padding: '16px 24px',
    borderTop: '1px solid rgba(var(--center-channel-color-rgb), 0.16)',
    display: 'flex',
    justifyContent: 'flex-end',
    gap: '8px',
};

// Elements a Tab-trap and initial-focus need to consider "focusable". Matches the
// common minimal set (no [disabled] filtering - disabled controls simply aren't
// selectable via keyboard already, so this stays a plain selector).
const FOCUSABLE_SELECTOR = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

// ModalShell is a minimal, dependency-free modal frame shared by every dialog in
// this section - this plugin's webapp does not pull in a design-system modal
// component elsewhere, so this stays consistent with that.
const ModalShell: React.FC<Props> = ({title, onClose, children, footer}) => {
    const dialogRef = useRef<HTMLDivElement>(null);

    useEffect(() => {
        const dialog = dialogRef.current;
        if (!dialog) {
            return undefined;
        }

        // Initial focus: land inside the dialog rather than leaving it on
        // whatever triggered the open, so screen readers announce it and Tab
        // starts cycling through its own controls instead of the page behind it.
        const initial = dialog.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)[0];
        (initial || dialog).focus();

        const handleKeyDown = (e: KeyboardEvent) => {
            if (e.key === 'Escape') {
                e.stopPropagation();
                onClose();
                return;
            }
            if (e.key !== 'Tab') {
                return;
            }

            // Focus trap: wrap Tab/Shift+Tab at the dialog's edges instead of
            // letting focus escape to the System Console page behind the overlay.
            const focusables = Array.from(dialog.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR));
            if (focusables.length === 0) {
                e.preventDefault();
                return;
            }
            const first = focusables[0];
            const last = focusables[focusables.length - 1];
            if (e.shiftKey && document.activeElement === first) {
                e.preventDefault();
                last.focus();
            } else if (!e.shiftKey && document.activeElement === last) {
                e.preventDefault();
                first.focus();
            }
        };

        document.addEventListener('keydown', handleKeyDown, true);
        return () => document.removeEventListener('keydown', handleKeyDown, true);
    }, [onClose]);

    return (
        <div
            style={overlayStyle}
            role='presentation'
            onClick={(e) => {
                if (e.target === e.currentTarget) {
                    onClose();
                }
            }}
        >
            <div
                ref={dialogRef}
                style={dialogStyle}
                role='dialog'
                aria-modal='true'
                aria-label={title}
                tabIndex={-1}
            >
                <div style={headerStyle}>
                    <h4 style={{margin: 0}}>{title}</h4>
                    <button
                        type='button'
                        style={{background: 'none', border: 'none', fontSize: '20px', cursor: 'pointer', lineHeight: 1, color: 'inherit'}}
                        aria-label='Close'
                        onClick={onClose}
                    >
                        {'×'}
                    </button>
                </div>
                <div style={bodyStyle}>
                    {children}
                </div>
                {footer && (
                    <div style={footerStyle}>
                        {footer}
                    </div>
                )}
            </div>
        </div>
    );
};

export default ModalShell;
