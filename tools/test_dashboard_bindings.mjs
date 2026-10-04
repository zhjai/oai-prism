import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';

const playwright = await import(process.env.PLAYWRIGHT_MODULE
  ? pathToFileURL(process.env.PLAYWRIGHT_MODULE).href : 'playwright');
const browser = await playwright.chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM });
const page = await browser.newPage({ viewport: { width: 1500, height: 1000 } });
const baseURL = process.env.OAIPRISM_BROWSER_URL;
if (!baseURL) throw new Error('Run through TestDashboardBrowser with its simulated accounts');
const adminKey = 'browser-test-admin-key';
const headers = { Authorization: `Bearer ${adminKey}`, 'Content-Type': 'application/json' };
const errors = [];
page.on('pageerror', (error) => errors.push(error.message));
await page.addInitScript(() => {
  if (!localStorage.getItem('oaiprism_api_key')) localStorage.setItem('oaiprism_api_key', 'browser-test-admin-key');
});
const api = async (method, path, body) => {
  const res = await fetch(`${baseURL}${path}`, { method, headers, body: body ? JSON.stringify(body) : undefined });
  assert.equal(res.status, 200);
  return res.json();
};
const waitUntil = async (check) => {
  for (let i = 0; i < 60; i++) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error('browser state failed to persist');
};
try {
  await page.goto(`${baseURL}/dashboard/`);
  await page.getByRole('menuitem', { name: '账号与计划池' }).click();
  const toggle = page.getByRole('switch', { name: 'Account B 启用状态' });
  await toggle.waitFor();
  await toggle.click();
  await waitUntil(async () => (await api('GET', '/admin/accounts')).accounts.find((a) => a.id === 'b').enabled === false);
  await waitUntil(async () => await page.getByRole('switch', { name: 'Account B 启用状态' }).getAttribute('aria-checked') === 'false');
  await page.reload();
  await page.getByRole('menuitem', { name: '账号与计划池' }).click();
  await page.getByRole('switch', { name: 'Account B 启用状态' }).waitFor();
  assert.equal(await page.getByRole('switch', { name: 'Account B 启用状态' }).getAttribute('aria-checked'), 'false');
  await page.getByRole('switch', { name: 'Account B 启用状态' }).click();
  await waitUntil(async () => (await api('GET', '/admin/accounts')).accounts.find((a) => a.id === 'b').enabled === true);

  await page.getByRole('button', { name: 'API 密钥' }).click();
  await page.getByPlaceholder('输入新 Key 描述名称 (例如: 生产环境客户端 / 本地Codex)').fill('Browser multi-account');
  await page.getByRole('combobox', { name: '新密钥绑定账号' }).click();
  await page.getByText('Account A (a)', { exact: true }).click();
  await page.getByText('Account B (b)', { exact: true }).click();
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '生成新 Key' }).click();
  let testKey;
  await waitUntil(async () => {
    testKey = (await api('GET', '/admin/apikeys')).find((k) => k.name === 'Browser multi-account');
    return testKey && testKey.account_ids.length === 2;
  });
  assert.deepEqual(testKey.account_ids, ['a', 'b']);
  const row = page.getByRole('row').filter({ hasText: 'Browser multi-account' });
  await row.getByRole('button', { name: '绑定账号', exact: true }).click();
  await page.getByRole('combobox', { name: '绑定账号选择' }).click();
  await page.getByText('Account C (c)', { exact: true }).last().click();
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '保存绑定' }).click();
  await waitUntil(async () => (await api('GET', '/admin/apikeys')).find((k) => k.name === 'Browser multi-account').account_ids.length === 3);
  await page.keyboard.press('Escape');
  const keyModal = page.locator('.ant-modal').filter({ hasText: '对外 API Key 管理与接入指引' });
  if (await keyModal.count()) await keyModal.locator('.ant-modal-footer button').last().click();

  await page.getByRole('menuitem', { name: 'Chat 调试台' }).click();
  await page.getByRole('combobox', { name: '调试账号' }).click();
  await page.getByText('Account B (b)', { exact: true }).click();
  await waitUntil(async () => (await api('GET', '/admin/chat/sessions')).some((s) => s.account_id === 'b'));
  const send = page.waitForRequest((r) => r.url().endsWith('/v1/chat/completions') && r.method() === 'POST');
  await page.getByPlaceholder('输入调试指令，例如：生成一个鹈鹕骑自行车的 SVG，用 HTML 实现...').fill('browser account selection smoke');
  await page.getByPlaceholder('输入调试指令，例如：生成一个鹈鹕骑自行车的 SVG，用 HTML 实现...').press('Enter');
  const request = await send;
  assert.equal(request.headers()['x-oaiprism-account'], 'b');
  await page.getByText('你好，这是一段流式回答。（完）', { exact: true }).waitFor();
  // The account can be disabled by an administrator after the selector loaded.
  // The web Chat must render the server's SSE error instead of a false success.
  await api('PUT', '/admin/accounts/b', { enabled: false });
  await page.getByPlaceholder('输入调试指令，例如：生成一个鹈鹕骑自行车的 SVG，用 HTML 实现...').fill('disabled account smoke');
  await page.getByPlaceholder('输入调试指令，例如：生成一个鹈鹕骑自行车的 SVG，用 HTML 实现...').press('Enter');
  await page.getByText('[请求失败] 指定账号已停用或暂不可用: b', { exact: true }).waitFor();
  await api('PUT', '/admin/accounts/b', { enabled: true });
  await api('PUT', `/admin/apikeys/${encodeURIComponent(testKey.key)}/bindings`, { account_ids: ['b'] });
  await page.reload();
  await page.getByRole('menuitem', { name: 'Chat 调试台' }).click();
  await page.getByRole('combobox', { name: '调试账号' }).waitFor();
  assert.ok(await page.getByText('Account B (b)', { exact: true }).count());
  await page.getByRole('combobox', { name: '调试账号' }).click();
  assert.equal(await page.getByText('Account A (a)', { exact: true }).count(), 0);
  assert.equal(await page.getByText('Account C (c)', { exact: true }).count(), 0);
  await page.keyboard.press('Escape');
  assert.deepEqual(errors, []);
  console.log('PASS: account toggle, multi-account key creation/edit, web Chat selection, restricted options, request header, inference, SSE failure, reload persistence');
} finally {
  await browser.close();
}
