// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

// generateToken mints a token client-side in the same RFC-4122 v4 UUID shape
// Mattermost's system console has always produced for a "generated" secret
// setting (this plugin's own now-removed matrix_as_token/matrix_hs_token fields
// used that exact setting type before the multi-server config moved these
// tokens into the server-management API) - an admin who is used to that
// Regenerate button sees the same token format here.
export function generateToken(): string {
    let id = 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx';
    id = id.replace(/[xy]/g, (c) => {
        const r = Math.floor(Math.random() * 16);
        const v = c === 'x' ? r : (r & 0x3) | 0x8;
        return v.toString(16);
    });
    return id;
}
