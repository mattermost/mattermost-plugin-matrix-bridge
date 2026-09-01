// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

// import '@mattermost/webapp/tests/setup';

import {webcrypto} from 'crypto';

import '@testing-library/jest-dom';

// jsdom 16 exposes no global `crypto`, which every supported browser has. Back
// it with Node's Web Crypto so code using crypto.getRandomValues (see
// src/utils/generate_token.ts) runs under test as it does in a browser.
if (!globalThis.crypto) {
    Object.defineProperty(globalThis, 'crypto', {value: webcrypto, configurable: true});
}

export {};
