// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import React from 'react';

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
    backgroundColor: 'white',
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
    padding: '16px 20px',
    borderBottom: '1px solid #ddd',
};

const bodyStyle: React.CSSProperties = {
    padding: '16px 20px',
};

const footerStyle: React.CSSProperties = {
    padding: '12px 20px',
    borderTop: '1px solid #ddd',
    display: 'flex',
    justifyContent: 'flex-end',
    gap: '8px',
};

// ModalShell is a minimal, dependency-free modal frame shared by every dialog in
// this section - this plugin's webapp does not pull in a design-system modal
// component elsewhere, so this stays consistent with that.
const ModalShell: React.FC<Props> = ({title, onClose, children, footer}) => {
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
                style={dialogStyle}
                role='dialog'
                aria-modal='true'
                aria-label={title}
            >
                <div style={headerStyle}>
                    <h4 style={{margin: 0}}>{title}</h4>
                    <button
                        type='button'
                        style={{background: 'none', border: 'none', fontSize: '20px', cursor: 'pointer', lineHeight: 1}}
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
