# screenshot-node

Headless-browser screenshot service, built on Playwright. Exercises
Lambdary's outgoing binary-body mapping (a PNG image, `isBase64Encoded`) and
query-string parsing.

`?url=` is attacker-controllable input from anyone who can reach this
endpoint, so the handler DNS-resolves and blocks navigation (including
redirects) to private/loopback/link-local addresses — including
`localhost`/`127.0.0.1` and the `169.254.169.254` cloud metadata address.
Point it at a real public URL.

## Setup

```sh
npm install
npx playwright install chromium
```

## Try it

From the `examples/` folder:

```sh
lambdary dev
curl "http://127.0.0.1:8000/screenshot?url=https://example.com" -o screenshot.png
curl "http://127.0.0.1:8000/screenshot?url=https://example.com&fullPage=true" -o screenshot-full.png
open screenshot.png   # or: xdg-open screenshot.png
```
