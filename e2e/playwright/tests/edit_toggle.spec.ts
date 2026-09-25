import {type Page} from '@playwright/test';

import {dialogButton, expectAllChecksOk, openMatrixSection, openServerMenuItem, serverRow, serverStatus} from '../support/console';
import type {E2EEnv} from '../support/env';
import {expect, test} from '../support/fixtures';
import {listServers, uniqueSuffix} from '../support/mattermost';

// The harness registers the server without a prefix, so it has the plugin's default.
const defaultUsernamePrefix = 'matrix';

async function editServer(page: Page, env: E2EEnv, field: string, value: string): Promise<void> {
    await openServerMenuItem(serverRow(page, env.server_id), 'Edit');
    const dialog = page.getByRole('dialog', {name: `Edit ${env.synapse_server_name}`});
    await dialog.getByLabel(field, {exact: true}).fill(value);
    await dialogButton(dialog, 'Save').click();
    await expect(dialog.getByText('Server updated.')).toBeVisible();
    await dialogButton(dialog, 'Done').click();
    await expect(dialog).toBeHidden();
}

test.describe('default env: edit and enable/disable', () => {
    // Restores the shared server to its launcher state even when a test fails midway, and waits
    // until Test is all ok again so later specs see a healthy server.
    test.afterEach(async ({api, env}) => {
        const patch = await api.pluginRequest('PATCH', `/servers/${env.server_id}`, {
            username_prefix: defaultUsernamePrefix,
            as_token: env.as_token,
        });
        expect(patch.ok(), `restore server: status ${patch.status()}`).toBe(true);
        const enable = await api.pluginRequest('PUT', `/servers/${env.server_id}/enabled`, {enabled: true});
        expect(enable.ok(), `re-enable server: status ${enable.status()}`).toBe(true);

        await expect.poll(async () => {
            const resp = await api.pluginRequest('POST', `/servers/${env.server_id}/test`);
            const {checks} = await resp.json();
            return checks.every((check: {status: string}) => check.status === 'ok');
        }, {message: 'Test is all ok after restoring the server', timeout: 30_000}).toBe(true);
        expect(await listServers(api)).toEqual([expect.objectContaining({
            server_id: env.server_id,
            server_name: env.synapse_server_name,
            server_url: env.synapse_internal_url,
            remote_id: env.remote_id,
            username_prefix: defaultUsernamePrefix,
            enabled: true,
        })]);
    });

    test('a new username prefix persists after reload', async ({page, env}) => {
        const prefix = `e2e${uniqueSuffix()}`;
        await openMatrixSection(page, env);
        await editServer(page, env, 'Username prefix', prefix);

        await page.reload();
        await openServerMenuItem(serverRow(page, env.server_id), 'Edit');
        const dialog = page.getByRole('dialog', {name: `Edit ${env.synapse_server_name}`});
        await expect(dialog.getByLabel('Username prefix', {exact: true})).toHaveValue(prefix);
    });

    test('a wrong AS token fails Test and marks the server unhealthy until restored', async ({page, env}) => {
        const row = serverRow(page, env.server_id);
        const testDialog = page.getByRole('dialog', {name: `Test ${env.synapse_server_name}`});
        await openMatrixSection(page, env);
        await editServer(page, env, 'Application Service token', `wrong-${uniqueSuffix()}`);

        await openServerMenuItem(row, 'Test connection');
        const check = (label: string) => testDialog.getByRole('listitem').filter({has: page.getByText(label, {exact: true})});
        await expect(check('Connection')).toContainText('❌');
        await expect(check('Application Service')).toContainText('(skipped)');
        await dialogButton(testDialog, 'Close').click();

        await page.getByRole('button', {name: 'Refresh'}).click();
        await expect(serverStatus(row)).toHaveText('Unhealthy');

        await editServer(page, env, 'Application Service token', env.as_token);
        await openServerMenuItem(row, 'Test connection');
        await expectAllChecksOk(testDialog);
        await dialogButton(testDialog, 'Close').click();
    });

    test('disabling and enabling apply without Save and persist after reload', async ({page, env}) => {
        const row = serverRow(page, env.server_id);
        const consoleSave = page.getByRole('button', {name: 'Save', exact: true});
        await openMatrixSection(page, env);
        await expect(consoleSave).toBeDisabled();

        // The row updates optimistically, so the PUT must finish before the reload.
        const disabled = page.waitForResponse((resp) => resp.url().endsWith(`/servers/${env.server_id}/enabled`));
        await openServerMenuItem(row, 'Disable connection');
        expect((await disabled).ok(), 'disable request').toBe(true);
        await expect(serverStatus(row)).toHaveText('Disabled');
        await expect(consoleSave).toBeDisabled();

        await page.reload();
        await expect(serverStatus(row)).toHaveText('Disabled');

        // The pill also reads Active while health is unknown, so the page must receive a healthy
        // probe. Earlier probes of the disabled server may still land, so only a healthy one counts.
        const healthy = page.waitForResponse(async (resp) => resp.url().endsWith('/api/v1/servers/health') && resp.ok() &&
            (await resp.json()).health?.[env.server_id] === 'healthy', {timeout: 15_000});
        await openServerMenuItem(row, 'Enable connection');
        await healthy;
        await expect(serverStatus(row)).toHaveText('Active');
        await expect(consoleSave).toBeDisabled();
        await page.reload();
        await expect(serverStatus(row)).toHaveText('Active');
    });
});
