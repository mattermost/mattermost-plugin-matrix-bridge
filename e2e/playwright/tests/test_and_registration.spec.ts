import {readFile} from 'node:fs/promises';

import {type Locator} from '@playwright/test';

import {dialogButton, openMatrixSection, openServerMenuItem, serverRow} from '../support/console';
import {expect, test} from '../support/fixtures';

async function expectAllChecksOk(dialog: Locator): Promise<void> {
    const checks = dialog.getByRole('listitem');
    await expect(checks).toHaveText([/Server URL/, /Matrix Client/, /Connection/, /Application Service/]);
    for (const check of await checks.all()) {
        await expect(check).toContainText('✅');
        await expect(check).not.toContainText('❌');
    }
}

test('Test lists every diagnostic check as ok', async ({page, env}) => {
    await openMatrixSection(page, env);
    await openServerMenuItem(serverRow(page, env.server_id), 'Test connection');

    const dialog = page.getByRole('dialog', {name: `Test ${env.synapse_server_name}`});
    await expectAllChecksOk(dialog);

    const rerun = page.waitForResponse((resp) => resp.url().endsWith(`/servers/${env.server_id}/test`));
    await dialogButton(dialog, 'Run again').click();
    await rerun;
    await expectAllChecksOk(dialog);

    await dialogButton(dialog, 'Close').click();
    await expect(dialog).toBeHidden();
});

test.describe('registration', () => {
    test.use({permissions: ['clipboard-read', 'clipboard-write']});

    test('shows the YAML with the plugin URL, and copy and download match it', async ({page, env}) => {
        await openMatrixSection(page, env);
        await openServerMenuItem(serverRow(page, env.server_id), 'View registration');

        const dialog = page.getByRole('dialog', {name: `Application Service registration for ${env.synapse_server_name}`});
        // The YAML holds the tokens, so the assertions compare booleans to keep it out of reports.
        const yaml = (await dialog.getByText(/^id: mattermost-bridge-/).textContent()) ?? '';
        const urlLine = `url: ${env.site_url}/plugins/${env.plugin_id}`;
        expect(yaml.split('\n').includes(urlLine), `YAML has the line "${urlLine}"`).toBe(true);
        expect(yaml.includes(`id: mattermost-bridge-${env.server_id}`), 'YAML has the server ID').toBe(true);
        expect(yaml.includes('/_matrix/app/v1'), 'YAML contains /_matrix/app/v1').toBe(false);

        await dialogButton(dialog, 'Copy').first().click();
        await expect.poll(async () => (await page.evaluate(() => navigator.clipboard.readText())) === yaml, {
            message: 'clipboard holds the displayed YAML',
        }).toBe(true);

        const [download] = await Promise.all([page.waitForEvent('download'), dialogButton(dialog, 'Download').click()]);
        expect(download.suggestedFilename()).toBe(`mattermost-bridge-${env.server_id}.yaml`);
        expect((await readFile(await download.path(), 'utf8')) === yaml, 'downloaded file matches the displayed YAML').toBe(true);
    });
});
