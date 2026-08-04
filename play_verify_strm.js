// 真实浏览器验证（strm 页内播放）：加载验证页 → 后端解析 strm → ffprobe 探测
// → L2 重封装 → 浏览器 <video> 真正解码播放。检查标题变为 PASS_STRM。
const { chromium } = require('playwright');

const BASE = 'http://localhost:18098';

(async () => {
  const browser = await chromium.launch({
    executablePath: '/usr/bin/chromium',
    args: ['--no-sandbox', '--disable-dev-shm-usage', '--autoplay-policy=no-user-gesture-required'],
  });
  const page = await browser.newPage({ viewport: { width: 900, height: 700 } });
  const log = (s) => console.log(s);

  const consoleErrors = [], pageErrors = [], failedRequests = [];
  page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()); });
  page.on('pageerror', (e) => pageErrors.push(e.message));
  page.on('requestfailed', (r) => failedRequests.push(r.url() + ' :: ' + (r.failure() ? r.failure().errorText : '')));

  await page.goto(BASE + '/', { waitUntil: 'load' });

  // 等待结果标题（PASS_STRM / FAIL_*）
  let title = '';
  for (let i = 0; i < 40; i++) {
    title = await page.title();
    if (title.startsWith('PASS') || title.startsWith('FAIL')) break;
    await page.waitForTimeout(500);
  }
  const out = await page.$eval('#out', (e) => e.textContent).catch(() => '');
  log('--- 页面输出 ---');
  log(out);
  log('--- 标题结果: ' + title + ' ---');
  if (consoleErrors.length) log('consoleErrors: ' + consoleErrors.join(' | '));
  if (pageErrors.length) log('pageErrors: ' + pageErrors.join(' | '));
  if (failedRequests.length) log('failedRequests: ' + failedRequests.join(' | '));

  await browser.close();
  process.exit(title === 'PASS_STRM' ? 0 : 1);
})().catch((e) => { console.error('运行异常:', e); process.exit(2); });
