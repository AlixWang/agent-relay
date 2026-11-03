// Serve the real embedded console assets for the mobile layout test.
// API calls are intercepted by the Playwright test itself, so this server only
// needs to answer static files.
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const uiRoot = path.resolve(here, '../../internal/web/ui');
const port = Number(process.env.PORT || 4173);

const contentTypes = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
};

createServer(async (req, res) => {
  const url = new URL(req.url || '/', 'http://127.0.0.1');
  const pathname = decodeURIComponent(url.pathname === '/' ? '/index.html' : url.pathname);
  const file = path.resolve(uiRoot, '.' + pathname);
  if (!file.startsWith(uiRoot + path.sep)) {
    res.writeHead(403);
    res.end('forbidden');
    return;
  }
  try {
    let body = await readFile(file);
    if (pathname === '/index.html') {
      body = Buffer.from(body.toString('utf8').replaceAll('{{ASSET_V}}', 'mobile-test'));
    }
    res.writeHead(200, {
      'Content-Type': contentTypes[path.extname(pathname)] || 'application/octet-stream',
      'Cache-Control': 'no-store',
    });
    res.end(body);
  } catch {
    res.writeHead(404);
    res.end('not found');
  }
}).listen(port, '127.0.0.1', () => {
  console.log(`mobile test server: http://127.0.0.1:${port}`);
});
