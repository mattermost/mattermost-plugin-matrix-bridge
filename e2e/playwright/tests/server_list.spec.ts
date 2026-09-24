import {openMatrixSection, serverRow, serverStatus} from '../support/console';
import {expect, test} from '../support/fixtures';
import {createChannel, executeCommand} from '../support/mattermost';

test('the server row shows its name, ID, URL and live health', async ({page, env}) => {
    // The pill reads Active before health arrives too, so the health response is checked directly.
    const health = page.waitForResponse((resp) => resp.url().endsWith('/api/v1/servers/health'));
    await openMatrixSection(page, env);
    expect((await (await health).json()).health[env.server_id]).toBe('healthy');

    const row = serverRow(page, env.server_id);
    await expect(row).toContainText(env.synapse_server_name);
    await expect(row).toContainText(env.synapse_internal_url);
    await expect(serverStatus(row)).toHaveText('Active');
});

test('the mappings panel lists a bridged channel, read-only', async ({page, env, api}) => {
    const channel = await createChannel(api, env.team_name);
    const reply = await executeCommand(api, channel.id, `/matrix create E2E UI ${channel.name}`);
    await expect.poll(async () => {
        const resp = await api.pluginRequest('GET', `/servers/${env.server_id}/mappings?per_page=200`);
        const {mappings} = await resp.json();
        return mappings.some((mapping: {channel_id: string}) => mapping.channel_id === channel.id);
    }, {message: `channel was never bridged; /matrix create replied: ${reply}`, timeout: 30_000}).toBe(true);

    await openMatrixSection(page, env);
    await page.getByRole('button', {name: `Show bridged channels for ${env.synapse_server_name}`}).click();

    const mapping = page.getByTestId('matrix-mapping-row').filter({hasText: channel.display_name});
    await expect(mapping).toBeVisible();
    // Cells are Channel, Team and Matrix room, in that order.
    await expect(mapping.locator(':scope > div').nth(1)).toHaveText(env.team_name);
    await expect(mapping.getByRole('button')).toHaveCount(0);
    await expect(mapping.getByRole('link')).toHaveCount(0);
});
