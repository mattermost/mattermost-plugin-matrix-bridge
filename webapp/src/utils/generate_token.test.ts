// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

import {generateToken} from './generate_token';

describe('generateToken', () => {
    it('produces an RFC-4122 v4 UUID', () => {
        const token = generateToken();
        expect(token).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    });

    it('never repeats across calls', () => {
        const tokens = new Set(Array.from({length: 20}, () => generateToken()));
        expect(tokens.size).toBe(20);
    });

    // The shape assertions above pass just as well for a Math.random token, so
    // they cannot catch a regression back to a predictable generator. These are
    // bearer credentials - pin the source explicitly.
    it('draws from crypto.getRandomValues, not Math.random', () => {
        const getRandomValues = jest.spyOn(crypto, 'getRandomValues');
        const random = jest.spyOn(Math, 'random');

        generateToken();

        expect(getRandomValues).toHaveBeenCalled();
        expect(random).not.toHaveBeenCalled();

        getRandomValues.mockRestore();
        random.mockRestore();
    });
});
