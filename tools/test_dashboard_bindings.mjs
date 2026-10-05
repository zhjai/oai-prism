import assert from 'node:assert/strict';
import { pathToFileURL } from 'node:url';
import { mkdir } from 'node:fs/promises';

const playwright = await import(process.env.PLAYWRIGHT_MODULE
  ? pathToFileURL(process.env.PLAYWRIGHT_MODULE).href : 'playwright');
const browser = await playwright.chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM });
const page = await browser.newPage({ viewport: { width: 1500, height: 1000 } });
const baseURL = process.env.OAIPRISM_BROWSER_URL;
if (!baseURL) throw new Error('Run through TestDashboardBrowser with its simulated accounts');
const adminKey = 'browser-test-admin-key';
const headers = { Authorization: `Bearer ${adminKey}`, 'Content-Type': 'application/json' };
const errors = [];
const bootstrap = process.env.OAIPRISM_BROWSER_BOOTSTRAP === '1';
page.on('pageerror', (error) => errors.push(error.message));
if (!bootstrap) await page.addInitScript(() => {
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
  if (bootstrap) {
    await page.goto(`${baseURL}/dashboard/`);
    await page.getByRole('button', { name: /API 密钥/ }).click();
    await page.getByLabel('新密钥名称', { exact: true }).fill('First browser key');
    const createdResponse = page.waitForResponse((r) => r.url().endsWith('/admin/apikeys') && r.request().method() === 'POST');
    await page.getByRole('button', { name: '生成新 Key' }).click();
    const created = await (await createdResponse).json();
    assert.ok(created.key);
    await waitUntil(async () => await page.evaluate(() => localStorage.getItem('oaiprism_api_key')) === created.key);
    const row = page.getByRole('row').filter({ hasText: 'First browser key' });
    await row.getByRole('button', { name: '显示密钥', exact: true }).waitFor();
    assert.equal((await row.innerText()).includes(created.key), false);
    assert.equal(await page.getByText('连接网关', { exact: true }).count(), 0);
    const accounts = await fetch(`${baseURL}/admin/accounts`, { headers: { Authorization: `Bearer ${created.key}` } });
    assert.equal(accounts.status, 200);
    await page.getByRole('dialog').getByRole('button', { name: '关闭', exact: true }).click();
    await page.reload();
    await page.getByRole('menuitem', { name: '账号与计划池' }).click();
    await page.getByRole('switch', { name: 'Account A 启用状态' }).waitFor();
    assert.equal(await page.getByText('连接网关', { exact: true }).count(), 0);
    assert.deepEqual(errors, []);
    console.log('PASS: first browser key is retained, authenticated and masked after bootstrap and reload');
  } else {
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

  // Failed refreshes must retain the last inventory and surface the failure.
  await page.route('**/admin/accounts', async (route) => {
    if (route.request().method() === 'GET') {
      await route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'synthetic inventory unavailable' }) });
    } else await route.continue();
  });
  await page.getByRole('button', { name: /重载并刷新/ }).click();
  await page.getByText('账号列表更新失败：synthetic inventory unavailable', { exact: true }).waitFor();
  assert.equal(await page.getByRole('switch', { name: 'Account A 启用状态' }).count(), 1);
  await page.unroute('**/admin/accounts');
  await page.getByRole('button', { name: '重试加载', exact: true }).click();
  await page.getByText('账号列表更新失败：synthetic inventory unavailable', { exact: true }).waitFor({ state: 'hidden' });

  const openImport = async () => {
    await page.getByRole('button', { name: /导入新账号/ }).click();
    await page.getByLabel('账号名称', { exact: true }).waitFor();
  };
  const submitImport = async () => {
    const [response] = await Promise.all([
      page.waitForResponse((r) => r.url().endsWith('/admin/accounts/import') && r.request().method() === 'POST'),
      page.getByRole('button', { name: /确认导入/ }).click(),
    ]);
    return response;
  };
  await openImport();
  assert.equal(await page.getByRole('checkbox', { name: '导入前校验凭据' }).isChecked(), true);
  await page.getByLabel('账号名称', { exact: true }).fill('Browser Direct Verified');
  await page.getByLabel('账号 ID（可选）', { exact: true }).fill('browser-direct-verified');
  await page.getByLabel('完整 Cookie', { exact: true }).fill('__Secure-next-auth.session-token=synthetic-valid; oai-did=synthetic-device');
  await page.getByLabel('最大并发数（0 = 不限）', { exact: true }).fill('0');
  await page.getByRole('switch', { name: '启用账号', exact: true }).click();
  const directResponse = await submitImport();
  assert.equal(directResponse.status(), 201);
  const directPayload = directResponse.request().postDataJSON();
  assert.equal(directPayload.verify, true);
  assert.equal(directPayload.accounts[0].plan, '');
  assert.equal(directPayload.accounts[0].max_concurrency, 0);
  assert.equal(directPayload.accounts[0].enabled, false);
  assert.equal(new URL(directResponse.request().url()).search, '');
  await page.getByRole('switch', { name: 'Browser Direct Verified 启用状态', exact: true }).waitFor();
  const imported = (await api('GET', '/admin/accounts')).accounts.find((a) => a.id === 'browser-direct-verified');
  assert.equal(imported.name, 'Browser Direct Verified');
  assert.equal(imported.plan, 'plus');
  assert.equal(imported.enabled, false);
  assert.equal(imported.max_concurrency, 0);
  assert.equal(imported.has_session, true);

  await openImport();
  assert.equal(await page.getByLabel('完整 Cookie', { exact: true }).inputValue(), '');
  assert.equal(await page.getByLabel('账号名称', { exact: true }).inputValue(), '');
  await page.getByLabel('账号名称', { exact: true }).fill('Browser Expired Cookie');
  await page.getByLabel('账号 ID（可选）', { exact: true }).fill('browser-direct-expired');
  await page.getByLabel('完整 Cookie', { exact: true }).fill('__Secure-next-auth.session-token=synthetic-expired');
  assert.equal((await submitImport()).status(), 502);
  await page.getByText(/登录凭据已失效或被撤销/).waitFor();
  await waitUntil(async () => await page.getByRole('checkbox', { name: '导入前校验凭据' }).isEnabled());
  assert.equal((await api('GET', '/admin/accounts')).accounts.some((a) => a.id === 'browser-direct-expired'), false);
  await page.getByRole('checkbox', { name: '导入前校验凭据' }).uncheck();
  assert.equal(await page.getByRole('checkbox', { name: '导入前校验凭据' }).isChecked(), false);
  await page.getByRole('switch', { name: '启用账号', exact: true }).click();
  assert.equal(await page.getByLabel('账号名称', { exact: true }).inputValue(), 'Browser Expired Cookie');
  assert.equal(await page.getByLabel('完整 Cookie', { exact: true }).inputValue(), '__Secure-next-auth.session-token=synthetic-expired');
  await page.getByRole('button', { name: /确认导入/ }).waitFor({ state: 'visible' });
  const skipped = await submitImport();
  assert.equal(skipped.status(), 201);
  assert.equal(skipped.request().postDataJSON().verify, false);
  await page.getByRole('switch', { name: 'Browser Expired Cookie 启用状态', exact: true }).waitFor();
  assert.equal(await page.evaluate(() => JSON.stringify({ ...localStorage }).includes('synthetic-expired')), false);

  await openImport();
  await page.getByRole('tab', { name: '文本 / JSON 粘贴' }).click();
  await page.getByLabel('批量账号凭据', { exact: true }).fill('{"cookies":"broken=value",}');
  let malformedSubmitted = false;
  const recordMalformed = (request) => {
    if (request.url().endsWith('/admin/accounts/import')) malformedSubmitted = true;
  };
  page.on('request', recordMalformed);
  await page.getByRole('button', { name: /确认导入/ }).click();
  await page.getByText('JSON 格式无效，请修正后重新导入', { exact: true }).waitFor();
  await waitUntil(async () => await page.getByRole('checkbox', { name: '导入前校验凭据' }).isEnabled());
  assert.equal(malformedSubmitted, false);
  page.off('request', recordMalformed);
  await page.getByLabel('批量账号凭据', { exact: true }).fill(JSON.stringify({ accounts: [{ id: 'browser-batch', name: 'Browser Batch', cookies: '__Secure-next-auth.session-token=synthetic-batch', enabled: false, max_concurrency: 0, weight: 3, rate_per_second: 0.5, rate_burst: 4, headers: { 'X-Synthetic': 'example' } }] }));
  const batchResponse = await submitImport();
  assert.equal(batchResponse.status(), 201);
  assert.equal(batchResponse.request().postDataJSON().accounts[0].weight, 3);
  assert.equal(batchResponse.request().postDataJSON().accounts[0].headers['X-Synthetic'], 'example');
  await page.getByRole('switch', { name: 'Browser Batch 启用状态', exact: true }).waitFor();

  await page.getByRole('button', { name: 'API 密钥' }).click();
  await page.getByLabel('填入已有 API Key', { exact: true }).fill('synthetic-invalid-key');
  await page.getByRole('button', { name: /使用此 Key/ }).click();
  await page.getByText(/这个 Key 无法通过网关校验/).waitFor();
  assert.equal(await page.evaluate(() => localStorage.getItem('oaiprism_api_key')), adminKey);
  assert.equal(await page.getByText('连接网关', { exact: true }).count(), 0);
  assert.equal(await page.getByLabel('填入已有 API Key', { exact: true }).inputValue(), 'synthetic-invalid-key');
  await page.getByLabel('填入已有 API Key', { exact: true }).fill('');
  await page.getByLabel('新密钥名称', { exact: true }).fill('Browser multi-account');
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
  assert.equal(await page.evaluate(() => localStorage.getItem('oaiprism_api_key')), adminKey);
  const row = page.getByRole('row').filter({ hasText: 'Browser multi-account' });
  assert.equal((await row.innerText()).includes(testKey.key), false);
  await row.getByRole('button', { name: '显示密钥', exact: true }).click();
  assert.equal((await row.innerText()).includes(testKey.key), true);
  await page.getByRole('tab', { name: '客户端接入代码示例', exact: true }).click();
  const examples = page.locator('.ant-modal pre');
  assert.equal((await examples.allTextContents()).join('\n').includes(adminKey), false);
  await page.getByRole('checkbox', { name: '在示例中使用真实密钥', exact: true }).check();
  assert.equal((await examples.allTextContents()).join('\n').includes(adminKey), true);
  await page.getByRole('dialog').getByRole('button', { name: '关闭', exact: true }).click();
  await page.getByRole('dialog').waitFor({ state: 'hidden' });
  await page.getByRole('button', { name: /API 密钥/ }).click();
  await row.getByRole('button', { name: '显示密钥', exact: true }).waitFor();
  assert.equal((await row.innerText()).includes(testKey.key), false);
  await page.evaluate(() => Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('synthetic clipboard denial'); } } }));
  await row.getByRole('button', { name: '复制密钥', exact: true }).click();
  await page.getByText(/复制失败：浏览器拒绝了剪贴板访问/).waitFor();
  assert.equal((await row.innerText()).includes(testKey.key), false);
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
  await waitUntil(async () => await page.getByRole('combobox', { name: '调试账号' }).isEnabled());
  // The account can be disabled by an administrator after the selector loaded.
  // The web Chat must render the server's SSE error instead of a false success.
  await api('PUT', '/admin/accounts/b', { enabled: false });
  const failedSend = page.waitForRequest((r) => r.url().endsWith('/v1/chat/completions') && r.method() === 'POST');
  await page.getByPlaceholder('输入调试指令，例如：生成一个鹈鹕骑自行车的 SVG，用 HTML 实现...').fill('disabled account smoke');
  await page.getByPlaceholder('输入调试指令，例如：生成一个鹈鹕骑自行车的 SVG，用 HTML 实现...').press('Enter');
  assert.equal((await failedSend).headers()['x-oaiprism-account'], 'b');
  await page.getByText('[请求失败] 指定账号已停用或暂不可用: b', { exact: true }).waitFor();
  await api('PUT', '/admin/accounts/b', { enabled: true });
  await api('PUT', `/admin/apikeys/${encodeURIComponent(testKey.key)}/bindings`, { account_ids: ['b'] });
  await page.evaluate((key) => localStorage.setItem('oaiprism_api_key', key), testKey.key);
  await page.reload();
  await page.getByRole('menuitem', { name: 'Chat 调试台' }).click();
  await page.getByRole('combobox', { name: '调试账号' }).waitFor();
  assert.ok(await page.getByText('Account B (b)', { exact: true }).count());
  await page.getByRole('combobox', { name: '调试账号' }).click();
  assert.equal(await page.getByText('Account A (a)', { exact: true }).count(), 0);
  assert.equal(await page.getByText('Account C (c)', { exact: true }).count(), 0);
  await page.keyboard.press('Escape');
  await page.evaluate((key) => localStorage.setItem('oaiprism_api_key', key), adminKey);
  await page.reload();
  await page.getByRole('menuitem', { name: '账号与计划池' }).click();
  const accountRow = page.getByRole('row').filter({ has: page.getByRole('switch', { name: 'Account C 启用状态' }) });
  await accountRow.getByRole('button').last().click();
  await page.getByRole('button', { name: '确定删除', exact: true }).click();
  await waitUntil(async () => !(await api('GET', '/admin/accounts')).accounts.some((a) => a.id === 'c'));
  const reloadRequest = page.waitForResponse((r) => r.url().endsWith('/admin/reload') && r.request().method() === 'POST');
  await page.getByRole('button', { name: /重载并刷新/ }).click();
  assert.equal((await reloadRequest).status(), 200);
  await page.reload();
  await page.getByRole('menuitem', { name: '账号与计划池' }).click();
  await page.getByRole('switch', { name: 'Account A 启用状态' }).waitFor();
  assert.equal(await page.getByRole('switch', { name: 'Account C 启用状态' }).count(), 0);
  assert.equal((await api('GET', '/admin/accounts')).accounts.some((a) => a.id === 'c'), false);
  if (process.env.OAIPRISM_SCREENSHOT) await page.screenshot({ path: process.env.OAIPRISM_SCREENSHOT, fullPage: true });
  if (process.env.OAIPRISM_SCREENSHOT_DIR) {
    const directory = process.env.OAIPRISM_SCREENSHOT_DIR;
    await mkdir(directory, { recursive: true });
    const waitForLayout = () => page.waitForFunction(() => !document.querySelector('.ant-spin-blur') && document.getAnimations().every((a) => a.playState !== 'running' || a.effect?.getTiming().iterations === Infinity));
    for (const mode of ['light', 'dark']) {
      await page.evaluate((value) => localStorage.setItem('oaiprism_theme', value), mode);
      for (const [name, width, height] of [['desktop', 1500, 1000], ['mobile', 390, 844]]) {
        await page.setViewportSize({ width, height });
        await page.reload();
        await page.getByRole('switch', { name: 'Account A 启用状态' }).waitFor();
        await waitForLayout();
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1), `${name} ${mode} overflows horizontally`);
        const table = page.locator('.ant-table-container');
        const tableBounds = await table.boundingBox();
        assert.ok(tableBounds && tableBounds.height >= 140, `${name} ${mode} account table collapsed`);
        await page.getByRole('switch', { name: 'Account A 启用状态' }).scrollIntoViewIfNeeded();
        const rowBounds = await page.getByRole('switch', { name: 'Account A 启用状态' }).boundingBox();
        assert.ok(rowBounds && rowBounds.y >= 0 && rowBounds.y + rowBounds.height <= height, `${name} ${mode} account row unreachable`);
        const accountCard = table.locator('xpath=ancestor::*[contains(concat(" ", normalize-space(@class), " "), " ant-card ")][1]');
        const cardBounds = await accountCard.boundingBox();
        const scrolledTableBounds = await table.boundingBox();
        assert.ok(cardBounds && scrolledTableBounds && scrolledTableBounds.y + scrolledTableBounds.height <= cardBounds.y + cardBounds.height + 1, `${name} ${mode} table extends outside its container`);
        const activeRow = page.getByRole('row').filter({ has: page.getByRole('switch', { name: 'Account A 启用状态' }) });
        const editAction = activeRow.getByRole('button', { name: '编辑', exact: true });
        await editAction.scrollIntoViewIfNeeded();
        const editBounds = await editAction.boundingBox();
        assert.ok(editBounds && editBounds.x >= 0 && editBounds.x + editBounds.width <= width, `${name} ${mode} account actions exceed viewport`);
        await page.screenshot({ path: `${directory}/${name}-${mode}-accounts.png`, fullPage: true });
        await page.getByRole('button', { name: /导入新账号/ }).click();
        await page.getByLabel('完整 Cookie', { exact: true }).waitFor();
        await waitForLayout();
        const modal = page.locator('.ant-modal').filter({ has: page.getByLabel('完整 Cookie', { exact: true }) });
        const bounds = await modal.boundingBox();
        assert.ok(bounds && bounds.width <= width, `${name} import modal exceeds viewport`);
        const titleBounds = await modal.locator('.ant-modal-title').boundingBox();
        assert.ok(titleBounds && titleBounds.y >= 0 && titleBounds.y < height, `${name} import modal title is offscreen`);
        const initialSubmitBounds = await page.getByRole('button', { name: /确认导入/ }).boundingBox();
        assert.ok(initialSubmitBounds && initialSubmitBounds.y >= 0 && initialSubmitBounds.y + initialSubmitBounds.height <= height, `${name} import footer is offscreen`);
        await page.getByLabel('完整 Cookie', { exact: true }).scrollIntoViewIfNeeded();
        await page.screenshot({ path: `${directory}/${name}-${mode}-import.png`, fullPage: true });
        const submit = page.getByRole('button', { name: /确认导入/ });
        await submit.scrollIntoViewIfNeeded();
        const submitBounds = await submit.boundingBox();
        assert.ok(submitBounds && submitBounds.y >= 0 && submitBounds.y + submitBounds.height <= height, `${name} import action unreachable`);
        await page.getByRole('button', { name: /^取\s*消$/ }).click();
        await page.getByLabel('完整 Cookie', { exact: true }).waitFor({ state: 'hidden' });
        await page.locator('header').getByRole('button').filter({ has: page.locator('[aria-label="key"]') }).click();
        const keysDialog = page.getByRole('dialog');
        await keysDialog.getByRole('tab', { name: '客户端接入代码示例', exact: true }).click();
        await waitForLayout();
        const keysTitle = await keysDialog.locator('.ant-modal-title').boundingBox();
        const keysFooter = await keysDialog.locator('.ant-modal-footer').boundingBox();
        assert.ok(keysTitle && keysTitle.y >= 0 && keysTitle.y + keysTitle.height <= height, `${name} ${mode} key modal title offscreen`);
        assert.ok(keysFooter && keysFooter.y >= 0 && keysFooter.y + keysFooter.height <= height, `${name} ${mode} key modal footer offscreen`);
        await page.screenshot({ path: `${directory}/${name}-${mode}-keys.png`, fullPage: true });
        await keysDialog.getByRole('button', { name: '关闭', exact: true }).click();
        await keysDialog.waitFor({ state: 'hidden' });
      }
    }
  }
  assert.deepEqual(errors, []);
  console.log('PASS: account toggle, inventory failure preservation, direct verified Cookie import, expired Cookie rejection, explicit skip verification, malformed JSON rejection, batch wrapper, management credential preservation, multi-account key creation/edit, web Chat selection, restricted options, request header, inference, SSE failure, reload persistence, account deletion after reload');
  }
} catch (error) {
  console.error('Browser failure:', error);
  if (process.env.OAIPRISM_SCREENSHOT_DIR) {
    await mkdir(process.env.OAIPRISM_SCREENSHOT_DIR, { recursive: true });
    await page.screenshot({ path: `${process.env.OAIPRISM_SCREENSHOT_DIR}/failure.png`, fullPage: true });
  }
  console.error('Browser modal geometry:', await page.locator('.ant-modal').evaluateAll((modals) => modals.map((modal) => ({
    modal: modal.getBoundingClientRect().toJSON(),
    panel: modal.querySelector('.ant-modal-container')?.getBoundingClientRect().toJSON(),
    footer: modal.querySelector('.ant-modal-footer')?.getBoundingClientRect().toJSON(),
  }))));
  if (await page.locator('.ant-modal').count()) console.error('Browser modal accessibility:', await page.locator('.ant-modal').first().ariaSnapshot());
  console.error('Browser page state:', await page.locator('body').innerText());
  throw error;
} finally {
  await browser.close();
}
