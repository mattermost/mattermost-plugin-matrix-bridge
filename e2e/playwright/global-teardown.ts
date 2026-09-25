import {existsSync, readFileSync, rmSync} from 'node:fs';
import {setTimeout as sleep} from 'node:timers/promises';

import {pidsFile} from './support/env';

const stopTimeoutMs = 3 * 60 * 1000;

export default async function globalTeardown(): Promise<void> {
    if (!existsSync(pidsFile)) {
        return;
    }
    await stopLaunchers(JSON.parse(readFileSync(pidsFile, 'utf8')) as number[]);
    rmSync(pidsFile);
}

// stopLaunchers sends SIGTERM so each launcher tears its containers down, and SIGKILLs any that
// are still running after stopTimeoutMs.
export async function stopLaunchers(pids: number[]): Promise<void> {
    pids.forEach((pid) => signal(pid, 'SIGTERM'));
    const deadline = Date.now() + stopTimeoutMs;
    while (pids.some(isRunning) && Date.now() < deadline) {
        await sleep(1000);
    }
    for (const pid of pids.filter(isRunning)) {
        console.warn(`e2e-env ${pid} did not stop within ${stopTimeoutMs / 1000}s; killing it`);
        signal(pid, 'SIGKILL');
    }
}

function isRunning(pid: number): boolean {
    try {
        process.kill(pid, 0);
        return true;
    } catch {
        return false;
    }
}

function signal(pid: number, sig: NodeJS.Signals): void {
    try {
        process.kill(pid, sig);
    } catch {
        // Already exited.
    }
}
