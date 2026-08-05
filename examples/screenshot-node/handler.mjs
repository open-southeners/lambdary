// GET /screenshot?url=https://example.com[&fullPage=true]
//
// Launches headless Chromium via Playwright and returns a PNG screenshot of
// the requested page as a binary Function URL response — exercises
// Lambdary's outgoing isBase64Encoded/binary-body mapping and query-string
// parsing (see event.queryStringParameters below).
//
// Since the target URL is attacker-controllable (anyone who can reach this
// endpoint picks where the browser navigates), every navigation — including
// redirects — is DNS-checked against private/loopback/link-local ranges
// first, so this can't be used to probe internal services or cloud
// metadata endpoints (e.g. 169.254.169.254) from wherever it's running.
import dns from 'node:dns';
import net from 'node:net';
import { chromium } from 'playwright';

const DEFAULT_URL = 'https://example.com';
const NAV_TIMEOUT_MS = 20_000;

function isForbiddenAddress(address) {
  const family = net.isIP(address);

  if (family === 4) {
    const [a, b] = address.split('.').map(Number);
    if (a === 127 || a === 10 || a === 0) return true;
    if (a === 172 && b >= 16 && b <= 31) return true;
    if (a === 192 && b === 168) return true;
    if (a === 169 && b === 254) return true; // link-local + cloud metadata (169.254.169.254)
    return false;
  }

  if (family === 6) {
    const a = address.toLowerCase();
    if (a === '::1' || a === '::') return true;
    if (a.startsWith('fc') || a.startsWith('fd')) return true; // fc00::/7 (unique local)
    if (/^fe[89ab]/.test(a)) return true; // fe80::/10 (link-local)
    return false;
  }

  return true; // couldn't classify it — block rather than risk it
}

async function hostnameIsForbidden(hostname) {
  let records;
  try {
    records = await dns.promises.lookup(hostname, { all: true, verbatim: true });
  } catch {
    return true; // unresolvable — block rather than risk it
  }
  return records.length === 0 || records.some((r) => isForbiddenAddress(r.address));
}

export const handler = async (event) => {
  const query = event.queryStringParameters ?? {};
  const requested = query.url ?? DEFAULT_URL;
  const fullPage = query.fullPage === 'true';

  let target;
  try {
    target = new URL(requested);
  } catch {
    return { statusCode: 400, body: `invalid url: ${requested}` };
  }
  if (target.protocol !== 'http:' && target.protocol !== 'https:') {
    return { statusCode: 400, body: `unsupported protocol: ${target.protocol}` };
  }
  if (await hostnameIsForbidden(target.hostname)) {
    return { statusCode: 400, body: `refusing to navigate to a private/internal address: ${target.hostname}` };
  }

  const browser = await chromium.launch();
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });

    // Re-check on every request the page makes, not just the initial URL —
    // a redirect could otherwise point the browser at a private address
    // after this function's own check has already passed.
    await page.route('**/*', async (route) => {
      const reqUrl = new URL(route.request().url());
      if (await hostnameIsForbidden(reqUrl.hostname)) {
        return route.abort();
      }
      return route.continue();
    });

    await page.goto(target.href, { waitUntil: 'networkidle', timeout: NAV_TIMEOUT_MS });
    const png = await page.screenshot({ fullPage });

    return {
      statusCode: 200,
      headers: { 'content-type': 'image/png' },
      isBase64Encoded: true,
      body: png.toString('base64'),
    };
  } finally {
    await browser.close();
  }
};
