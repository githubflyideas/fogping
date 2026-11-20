// Browser test for the sign-in page's Caps Lock hint, against the real Go
// router (so the page's CSP applies). Run: cd browser && npm ci && npm test
const {test} = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');

function startServer() {
  const child = spawn('go', ['test', '-count=1', '-run', '^TestBrowserServer$'], {
    cwd: path.join(__dirname, '..'),
    env: {...process.env, BROWSER_TEST_SERVE: '1'},
    stdio: ['ignore', 'pipe', 'inherit'],
    detached: true, // its own process group, so the test binary stops with it
  });
  return new Promise((resolve, reject) => {
    let out = '';
    const timer = setTimeout(() => reject(new Error('server did not start\n' + out)), 120000);
    child.stdout.on('data', b => {
      out += b;
      const m = out.match(/BROWSER_TEST_URL=(\S+)/);
      if (m) { clearTimeout(timer); resolve({child, url: m[1]}); }
    });
    child.on('exit', code => reject(new Error('server exited ' + code + '\n' + out)));
  });
}

// Chromium cannot toggle the real Caps Lock from a test, so the events carry
// the modifier state the way a browser reports it.
const key = (page, sel, type, caps) => page.locator(sel).evaluate((el, [type, caps]) =>
  el.dispatchEvent(new KeyboardEvent(type, {key: caps ? 'A' : 'a', bubbles: true, modifierCapsLock: caps})),
  [type, caps]);

test('login page shows a Caps Lock hint only while it is on', {timeout: 180000}, async () => {
  const {child, url} = await startServer();
  const browser = await chromium.launch();
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
    page.on('pageerror', e => errors.push(e.message));
    await page.goto(url + '/login');
    const hint = page.locator('#caps');
    assert.equal(await hint.getAttribute('aria-live'), 'polite');
    assert.match(await page.locator('#pass').getAttribute('aria-describedby'), /\bcaps\b/);
    assert.equal(await hint.textContent(), '');

    await page.locator('#pass').focus();
    await key(page, '#pass', 'keydown', true);
    assert.match(await hint.textContent(), /\S/);
    assert.ok(await hint.isVisible(), 'hint is visible');

    await key(page, '#pass', 'keyup', false);
    assert.equal(await hint.textContent(), '');

    // On, then focus moves away: hidden; back to the password field: shown.
    await key(page, '#pass', 'keydown', true);
    await page.locator('#user').focus();
    assert.equal(await hint.textContent(), '');
    await page.locator('#pass').focus();
    assert.match(await hint.textContent(), /\S/);

    // Typing in the username field with Caps Lock on shows nothing there.
    await page.locator('#user').focus();
    await key(page, '#user', 'keydown', true);
    assert.equal(await hint.textContent(), '');

    assert.deepEqual(errors, [], 'no console errors (a CSP violation would be one)');
  } finally {
    await browser.close();
    process.kill(-child.pid, 'SIGTERM');
  }
});
