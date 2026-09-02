// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

export {};

describe('Plugin.initialize', () => {
    it('registers matrix_servers and no longer registers registration_download or homeserver_config', async () => {
        // index.tsx calls window.registerPlugin as a module-load side effect, so it
        // must be stubbed before the module is (dynamically) required below.
        (window as unknown as {registerPlugin: jest.Mock}).registerPlugin = jest.fn();

        // eslint-disable-next-line global-require, @typescript-eslint/no-var-requires
        const Plugin = require('./index').default;

        const registry = {
            registerAdminConsoleCustomSection: jest.fn(),
            registerAdminConsoleCustomSetting: jest.fn(),
            registerPostTypeComponent: jest.fn(),
        };

        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        await new Plugin().initialize(registry as any, {} as any);

        expect(registry.registerAdminConsoleCustomSection).toHaveBeenCalledTimes(1);
        expect(registry.registerAdminConsoleCustomSection).toHaveBeenCalledWith('matrix_servers', expect.anything());
        expect(registry.registerAdminConsoleCustomSetting).not.toHaveBeenCalled();
    });
});
