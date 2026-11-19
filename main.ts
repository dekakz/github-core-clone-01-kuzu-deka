// github-core-clone-01 entrypoint: the forge server lifecycle only — the forge itself is the
// statically linked Go binary ./forge beside this file (Go source in *.go, built with
// CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"). This launcher spawns it, reads the
// ephemeral port it bound from its stdout, and runs the env lifecycle around it.
// The entry <-> env lifecycle, as env/src/wire.ts settles it: bind an ephemeral port, tell the env the
// port it bound, then poll until the env says the trial is over and exit 0. Keep this file as it is:
// the suite scores the server the forge builds, and the lifecycle is the harness's, not the contest's.
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial container). Without it
//      there is nothing to talk to, so the program exits 1 at once.
//   2. Spawn ./forge: it binds an ephemeral port itself and prints "[forge] listening on port N".
//      Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address the request came from
//      plus N as your base URL and dials it from then on. The first register wins; a 409 means another
//      entry already registered, and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env answers at once ({"done":
//      false} while the suite runs), so this is a poll, not a long-held request. A failed or slow poll
//      is retried with a short exponential backoff; the process never exits before the env says done.
//   5. Stop the forge and exit 0. The harness then stops the env, which writes the export the scorer
//      reads.
//
// main.ts spawns its own server and nothing else: no package, no model call.

import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** How long one register or done request may take before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** The first wait between retries, and the longest. */
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** The wait after `attempt` failed requests: exponential from RETRY_INITIAL_MS, capped. */
function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

/**
 * POST the bound port until the env accepts it. 200 is the normal answer; 409 means a first register
 * already won, which is not this process's to fix. 400 and 405 are contract errors and end the program;
 * a network error or another status is retried, since the env may still be starting.
 */
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

/** Poll GET /done until it answers {"done":true}; never return before that. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

/** How long the forge may take to print its port before the entry gives up. */
const FORGE_START_TIMEOUT_MS = 10_000;

/**
 * Spawn ./forge (beside this file) and wait for its "[forge] listening on port N" line.
 * The forge binds the ephemeral port itself, so there is no bind-then-hand-over race.
 */
async function startForge(): Promise<{ child: ChildProcess; port: number }> {
  const dir = dirname(fileURLToPath(import.meta.url));
  const child = spawn(join(dir, 'forge'), [], {
    stdio: ['ignore', 'pipe', 'inherit'],
    env: { ...process.env },
  });
  const port = await new Promise<number>((resolve, reject) => {
    let buf = '';
    const timer = setTimeout(() => reject(new Error('forge did not report a port within 10s')), FORGE_START_TIMEOUT_MS);
    child.stdout?.on('data', (chunk: Buffer) => {
      buf += chunk.toString('utf8');
      const m = /\[forge\] listening on port (\d+)/.exec(buf);
      if (m) {
        clearTimeout(timer);
        resolve(Number(m[1]));
      }
    });
    child.once('error', (err) => {
      clearTimeout(timer);
      reject(err);
    });
    child.once('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`forge exited early with code ${code}`));
    });
  });
  return { child, port };
}

/** Start the forge, register its port, then wait for the env: the whole life of the process. */
async function main(): Promise<void> {
  const { child, port } = await startForge();
  process.stdout.write(`[entry] listening on port ${port}\n`);

  try {
    await register(port);
    process.stdout.write('[entry] registered; waiting for the env to say done\n');

    await waitForDone();
  } finally {
    child.kill('SIGTERM');
  }
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
