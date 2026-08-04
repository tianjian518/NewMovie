// 真实浏览器验证：海报经 /api/image 服务端代理后，<img> 能真正渲染（naturalWidth>0）。
const { chromium } = require('playwright');
const BASE = 'http://localhost:18098';

(async () => {
  const browser = await chromium.launch({
    executablePath: '/usr/bin/chromium',
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const failed = [];
  page.on('requestfailed', (r) => failed.push(r.url() + ' :: ' + (r.failure() ? r.failure().errorText : '')));
  const log = (s) => console.log(s);

  await page.goto(BASE + '/', { waitUntil: 'networkidle' });
  await page.waitForTimeout(600);
  await page.fill('input[placeholder="用户名"], input[type="text"]', 'admin');
  await page.fill('input[placeholder="密码"], input[type="password"]', 'admin');
  await page.click('button[type="submit"], form button');
  await page.waitForTimeout(1800);

  // 等海报 <img> 出现并加载
  await page.waitForSelector('img[src*="/api/image"]', { timeout: 15000 }).catch(() => log('[warn] 未找到 /api/image 海报'));
  await page.waitForTimeout(2500);

  const info = await page.evaluate(() => {
    const imgs = Array.from(document.querySelectorAll('img[src*="/api/image"]'));
    return imgs.map((im) => ({
      src: im.src.slice(0, 70),
      naturalWidth: im.naturalWidth,
      naturalHeight: im.naturalHeight,
      complete: im.complete,
    }));
  });

  await page.screenshot({ path: '/workspace/play_verify_poster.png', fullPage: false });

  const rendered = info.filter((i) => i.naturalWidth > 0 && i.complete);
  const ok = rendered.length > 0 && failed.filter((f) => f.includes('/api/image')).length === 0;
  log('海报 <img> 数量: ' + info.length);
  info.forEach((i) => log('  ' + JSON.stringify(i)));
  log('成功渲染(有像素): ' + rendered.length);
  log('失败请求: ' + (failed.length ? JSON.stringify(failed.slice(0, 5)) : '无'));
  log('=== 判定: ' + (ok ? 'PASS ✅ 海报经 /api/image 代理真实渲染' : 'FAIL ❌') + ' ===');
  await browser.close();
  process.exit(ok ? 0 : 1);
})().catch((e) => { console.error('SCRIPT ERROR:', e); process.exit(2); });
