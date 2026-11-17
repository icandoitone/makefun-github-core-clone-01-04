// The entry <-> env lifecycle: start the Go forge, learn its ephemeral port,
// register it with the env, then poll until the env says the trial is over.
// Node 22, standard library only. No model call, no package.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

async function main(): Promise<void> {
  const dir = dirname(fileURLToPath(import.meta.url));
  const forge = join(dir, 'forge');
  const child = spawn(forge, [], { stdio: ['ignore', 'pipe', 'inherit'], env: process.env });
  let port = 0;
  let buffer = '';
  const portPromise = new Promise<number>((resolve, reject) => {
    child.stdout!.on('data', (chunk: Buffer) => {
      buffer += chunk.toString('utf8');
      const match = /PORT=(\d+)/.exec(buffer);
      if (match !== null) resolve(Number(match[1]));
    });
    child.once('error', reject);
    child.once('exit', (code) => reject(new Error(`the forge exited early with code ${code}`)));
  });
  port = await portPromise;
  process.stdout.write(`[entry] forge listening on port ${port}\n`);

  if (ENV_URL === '') {
    // Local run: no env to register with. Keep the forge up and print the
    // port for the local runner; stop when the process is killed.
    process.stdout.write(`[entry] MAKEFUN_ENV_URL is not set: running locally on port ${port}\n`);
    await new Promise<void>((resolve) => {
      child.once('exit', () => resolve());
      process.on('SIGINT', () => { child.kill(); resolve(); });
      process.on('SIGTERM', () => { child.kill(); resolve(); });
    });
    return;
  }

  await register(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');
  await waitForDone();
  child.kill();
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
