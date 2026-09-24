import {dialogButton, openMatrixSection, openServerMenuItem, serverRow, serverRows} from '../support/console';
import {expect, test} from '../support/fixtures';
import {listServers} from '../support/mattermost';

test.describe('no-server env: add and remove', () => {
    test.use({envName: 'noServer'});
    test.describe.configure({mode: 'serial'});

    // Leaves the env with no servers even when a test fails midway.
    test.afterEach(async ({api}) => {
        for (const server of await listServers(api)) {
            const resp = await api.pluginRequest('DELETE', `/servers/${server.server_id}`);
            expect(resp.ok(), `remove server: status ${resp.status()}`).toBe(true);
        }
        expect(await listServers(api)).toEqual([]);
    });

    test('adding a server discovers its name and offers the registration', async ({page, env, api}) => {
        await openMatrixSection(page, env);
        await expect(page.getByText('No Matrix servers are registered yet.')).toBeVisible();

        await page.getByRole('button', {name: 'Add a connection'}).click();
        const form = page.getByRole('dialog', {name: 'Add Matrix server'});
        await form.getByLabel('Homeserver URL', {exact: true}).fill(env.synapse_internal_url);
        await form.getByLabel('Application Service token', {exact: true}).fill(env.as_token);
        await form.getByLabel('Homeserver token', {exact: true}).fill(env.hs_token);
        await dialogButton(form, 'Add server').click();

        // server_name was never typed, so seeing it proves the plugin discovered it.
        const added = page.getByRole('dialog', {name: 'Matrix server added'});
        await expect(added).toContainText(`Registered as ${env.synapse_server_name}`);

        await dialogButton(added, 'View registration YAML').click();
        const registration = page.getByRole('dialog', {name: `Application Service registration for ${env.synapse_server_name}`});
        const yaml = (await registration.getByText(/^id: mattermost-bridge-/).textContent()) ?? '';
        expect(yaml.split('\n').some((line) => line.startsWith('url: ')), 'YAML has a url line').toBe(true);
        await dialogButton(registration, 'Close').click();
        await expect(registration).toBeHidden();

        const servers = await listServers(api);
        expect(servers.map((server) => server.server_name)).toEqual([env.synapse_server_name]);
        await expect(serverRow(page, servers[0].server_id)).toContainText(env.synapse_server_name);
    });

    test('removing a server shows its recovery key and restore command', async ({page, env, api}) => {
        const seeded = await api.pluginRequest('POST', '/servers', {
            server_url: env.synapse_internal_url,
            as_token: env.as_token,
            hs_token: env.hs_token,
        });
        expect(seeded.status(), 'seed server').toBe(201);
        const serverId: string = (await seeded.json()).server.server_id;

        await openMatrixSection(page, env);
        await openServerMenuItem(serverRow(page, serverId), 'Remove');

        const confirm = page.getByRole('dialog', {name: `Remove ${env.synapse_server_name}?`});
        await expect(confirm).toContainText(`Recovery key (server_id): ${serverId}`);
        await expect(confirm.locator('pre')).toHaveText(new RegExp(`--server-id ${serverId}$`));
        await dialogButton(confirm, 'Remove').click();

        const removed = page.getByRole('dialog', {name: 'Server removed'});
        await expect(removed.locator('pre')).toContainText(`--server-id ${serverId}`);
        await dialogButton(removed, 'Done').click();

        await expect(page.getByText('No Matrix servers are registered yet.')).toBeVisible();
        await expect(serverRows(page)).toHaveCount(0);
        expect(await listServers(api)).toEqual([]);
    });
});
