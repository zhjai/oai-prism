import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';

const { chromium } = await import(pathToFileURL(process.env.PLAYWRIGHT_MODULE).href);
const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM });
const page = await browser.newPage();
const base = process.env.OAIPRISM_BROWSER_URL;
const headers = { Authorization: 'Bearer browser-test-admin-key', 'Content-Type': 'application/json' };
try {
  await fetch(`${base}/admin/chat/sessions`, { method: 'POST', headers, body: JSON.stringify({ id: 'retired', title: 'Retired session', model: 'gpt-6.1-sol', reasoning_effort: 'high' }) });
  await page.addInitScript(() => localStorage.setItem('oaiprism_api_key', 'browser-test-admin-key'));
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  let fail = false;
  let inferenceCount = 0;
  page.on('request', (r) => { if (r.url().endsWith('/v1/chat/completions')) inferenceCount++; });
  await page.route('**/v1/models', async (route) => {
    const pin = route.request().headers()['x-oaiprism-account'];
    const models = pin === 'b' ? [{ id: 'model-b', name: 'Model B', default: true }] : [{ id: 'model-a', name: 'Model A', default: true }];
    await route.fulfill({ status: fail ? 503 : 200, contentType: 'application/json', body: JSON.stringify(fail ? { error: { message: 'Catalog offline', code: 'model_catalog_unavailable' } } : { object: 'list', data: models }) });
  });
  await page.goto(`${base}/dashboard/`);
  await page.getByRole('menuitem', { name: 'Chat 调试台' }).click();
  await page.locator('.ant-typography-warning').filter({ hasText: '之前的模型已不可用，请重新选择模型' }).waitFor();
  const input = page.getByPlaceholder('输入调试指令，例如：生成一个鹈鹕骑自行车的 SVG，用 HTML 实现...');
  await input.fill('must not use a retired model');
  await input.press('Enter');
  assert.equal(inferenceCount, 0);
  assert.equal(await input.inputValue(), 'must not use a retired model');
  await page.getByRole('button', { name: /选择可用模型/ }).click();
  await page.getByRole('menuitem', { name: /Model A/ }).click();
  await page.getByRole('button', { name: /Model A/ }).waitFor();
  await page.getByRole('combobox', { name: '调试账号' }).click();
  await page.getByText('Account B (b)', { exact: true }).click();
  await page.locator('.ant-typography-warning').filter({ hasText: '之前的模型已不可用，请重新选择模型' }).waitFor();
  await page.getByRole('button', { name: /选择可用模型/ }).click();
  assert.equal(await page.getByRole('menuitem', { name: /Model A/ }).count(), 0);
  await page.getByRole('menuitem', { name: /Model B/ }).click();
  const sent = page.waitForRequest((r) => r.url().endsWith('/v1/chat/completions'));
  await input.fill('available model smoke');
  await input.press('Enter');
  const request = await sent;
  assert.equal(request.postDataJSON().model, 'model-b');
  assert.equal(request.headers()['x-oaiprism-account'], 'b');
  await page.getByText('你好，这是一段流式回答。（完）', { exact: true }).waitFor();
  // Final text can render before [DONE] releases the streaming controls.
  for (let i = 0; i < 100 && !await page.getByRole('combobox', { name: '调试账号' }).isEnabled(); i++) {
    await page.waitForTimeout(100);
  }
  assert.equal(await page.getByRole('combobox', { name: '调试账号' }).isEnabled(), true);
  fail = true;
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await page.getByRole('button', { name: '刷新模型', exact: true }).waitFor();
  await input.fill('must not fabricate models during outage');
  await input.press('Enter');
  assert.equal(inferenceCount, 1);
  assert.equal(await input.inputValue(), 'must not fabricate models during outage');
  fail = false;
  await page.getByRole('button', { name: '刷新模型', exact: true }).click();
  await page.getByRole('button', { name: /Model B/ }).waitFor();
  // First-load outage with no sessions must recover to a sendable new session.
  const sessions = await (await fetch(`${base}/admin/chat/sessions`, { headers })).json();
  for (const session of sessions) await fetch(`${base}/admin/chat/sessions/${session.id}`, { method: 'DELETE', headers });
  fail = true;
  await page.reload();
  await page.getByRole('menuitem', { name: 'Chat 调试台' }).click();
  await page.getByRole('button', { name: '刷新模型', exact: true }).waitFor();
  fail = false;
  await page.getByRole('button', { name: '刷新模型', exact: true }).click();
  await page.getByRole('button', { name: /Model A/ }).waitFor();
  const recoveredRequest = page.waitForRequest((r) => r.url().endsWith('/v1/chat/completions'));
  await input.fill('send after first-load outage recovery');
  await input.press('Enter');
  assert.equal((await recoveredRequest).postDataJSON().model, 'model-a');
  await page.getByText('你好，这是一段流式回答。（完）', { exact: true }).waitFor();
  assert.deepEqual(errors, []);
  console.log('PASS: retired session requires explicit model selection, pinned catalog selection, usable request model/header, outage blocks sending without fabricated fallback, retry recovery, first-load outage recovery creates a sendable session');
} finally {
  await browser.close();
}
