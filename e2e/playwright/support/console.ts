import {expect, type Locator, type Page} from '@playwright/test';

import type {E2EEnv} from './env';

// openMatrixSection opens the plugin's System Console page and waits for the "Connected Matrix
// Servers" heading of its "Matrix homeservers" section.
export async function openMatrixSection(page: Page, env: E2EEnv): Promise<void> {
    await page.goto(`/admin_console/plugins/plugin_${env.plugin_id}`);
    await expect(page.getByRole('heading', {name: 'Connected Matrix Servers'})).toBeVisible();
}

export function serverRows(page: Page): Locator {
    return page.getByTestId('matrix-server-row');
}

export function serverRow(page: Page, serverId: string): Locator {
    return serverRows(page).filter({hasText: serverId});
}

// serverStatus is the row's status pill: Active, Disabled or Unhealthy.
export function serverStatus(row: Locator): Locator {
    return row.getByTestId('matrix-server-status');
}

export async function openServerMenuItem(row: Locator, item: string): Promise<void> {
    await row.getByRole('button', {name: /^Actions for /}).click();
    await row.getByRole('menuitem', {name: item}).click();
}

// dialogButton finds a dialog button by its visible text. It tells the footer "Close" apart from
// the header's "×" button, whose accessible name is also "Close".
export function dialogButton(dialog: Locator, text: string): Locator {
    return dialog.getByRole('button').filter({hasText: new RegExp(`^${text}$`)});
}

// expectAllChecksOk waits for a Test dialog's results and expects exactly the four diagnostic checks,
// in order, all ok.
export async function expectAllChecksOk(dialog: Locator): Promise<void> {
    const checks = dialog.getByRole('listitem');
    await expect(checks).toHaveText([/Server URL/, /Matrix Client/, /Connection/, /Application Service/]);
    for (const check of await checks.all()) {
        await expect(check).toContainText('✅');
        await expect(check).not.toContainText('❌');
    }
}
