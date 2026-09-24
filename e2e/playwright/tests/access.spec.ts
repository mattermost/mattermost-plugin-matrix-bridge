import {loginStorageState} from '../support/auth';
import {expect, test} from '../support/fixtures';
import {createUser} from '../support/mattermost';

test('a non-admin cannot reach the section', async ({browser, env, api}) => {
    const user = await createUser(api, env.team_name);
    const context = await browser.newContext({
        baseURL: env.mattermost_url,
        storageState: await loginStorageState(env.mattermost_url, user.username, user.password),
    });
    try {
        const page = await context.newPage();
        await page.goto(`/admin_console/plugins/plugin_${env.plugin_id}`);

        await expect(page).toHaveURL(new RegExp(`/${env.team_name}/channels/`));
        await expect(page.getByRole('textbox', {name: /write to/i})).toBeVisible();
        await expect(page.getByRole('heading', {name: 'Connected Matrix Servers'})).toHaveCount(0);
    } finally {
        await context.close();
    }
});
