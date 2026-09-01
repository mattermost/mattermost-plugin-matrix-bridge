// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

// generateToken mints a token client-side in the same RFC-4122 v4 UUID shape
// Mattermost's system console has always produced for a "generated" secret
// setting (this plugin's own now-removed matrix_as_token/matrix_hs_token fields
// used that exact setting type before the multi-server config moved these
// tokens into the server-management API) - an admin who is used to that
// Regenerate button sees the same token format here.
//
// The randomness comes from crypto.getRandomValues, not Math.random: these
// values are used verbatim as the as_token/hs_token bearer credentials that
// authenticate the homeserver against this bridge, so they must not come from a
// predictable generator.
export function generateToken(): string {
    const bytes = new Uint8Array(16);
    crypto.getRandomValues(bytes);

    bytes[6] = (bytes[6] & 0x0f) | 0x40; // version 4
    bytes[8] = (bytes[8] & 0x3f) | 0x80; // variant 10x

    const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
    return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
