import {openMatrixSection, serverRow, serverRows} from '../support/console';
import {expect, test} from '../support/fixtures';

test('the default env lists its registered server', async ({page, env}) => {
    await openMatrixSection(page, env);

    await expect(serverRow(page, env.server_id)).toBeVisible();
    await expect(serverRows(page)).toHaveCount(1);
});

test.describe('no-server env', () => {
    test.use({envName: 'noServer'});

    test('shows the empty state', async ({page, env}) => {
        await openMatrixSection(page, env);

        await expect(page.getByText('No Matrix servers are registered yet.')).toBeVisible();
        await expect(serverRows(page)).toHaveCount(0);
    });
});
