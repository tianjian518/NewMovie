// 真实浏览器端到端验证（2.0 播放修复）：
// 登录 → 分别打开 MP4（L0 直链，应走 /openlist 反代）与 MKV（L2 重封装）播放页
// → 检查 <video> 的 src 不再指向容器内部 127.0.0.1:5244，且视频真的能加载/推进。
const { chromium } = require('playwright');

const BASE = 'http://localhost:18097';
const CASES = [
  { id: 'f-c487ac4758d4fa9b', name: 'MP4原生(L0直链)', expectOpenlist: true },
  { id: 'f-2be79f91c5c21d73', name: 'MKV(L2重封装)', expectOpenlist: false },
];

(async () => {
  const browser = await chromium.launch({
    executablePath: '/usr/bin/chromium',
    args: ['--no-sandbox', '--disable-dev-shm-usage', '--autoplay-policy=no-user-gesture-required'],
  });
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });

  const consoleErrors = [], pageErrors = [], remuxStatus = [], failedRequests = [];
  page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text()); });
  page.on('pageerror', (e) => pageErrors.push(e.message));
  page.on('response', (r) => {
    const u = r.url();
    if (u.includes('/api/play/remux') || u.includes('/api/play/transcode')) remuxStatus.push(r.status() + ' ' + u.split('?')[0]);
    if (u.includes('/openlist/')) remuxStatus.push('OPENLIST ' + r.status() + ' ' + u.split('?')[0]);
  });
  page.on('requestfailed', (r) => failedRequests.push(r.url() + ' :: ' + (r.failure() ? r.failure().errorText : '')));
  const log = (s) => console.log(s);

  // 1) 登录
  await page.goto(BASE + '/', { waitUntil: 'networkidle' });
  await page.waitForTimeout(800);
  await page.fill('input[placeholder="用户名"], input[type="text"]', 'admin');
  await page.fill('input[placeholder="密码"], input[type="password"]', 'admin');
  await page.click('button[type="submit"], form button');
  await page.waitForTimeout(1500);
  log('1. 登录后标题: ' + (await page.title()));

  let allOk = true;

  for (const c of CASES) {
    log('\n===== 用例: ' + c.name + ' (' + c.id + ') =====');
    await page.goto(BASE + '/play/' + c.id, { waitUntil: 'networkidle' });
    await page.waitForTimeout(1500);
    await page.waitForSelector('video', { timeout: 15000 }).catch(() => log('   [warn] 未找到 <video>'));

    const reached = await page.waitForFunction(() => {
      const v = document.querySelector('video');
      return v && (v.readyState >= 2 || (v.error && v.error.code));
    }, { timeout: 25000 }).then(() => true).catch(() => false);

    // 到达可播状态后再测推进：先记起点，等 3.5s 再记终点。
    // 小体积测试片可能很快播完，故「推进」也接受「已到片尾（currentTime≈duration）」。
    const before = await page.evaluate(() => {
      const v = document.querySelector('video');
      return v ? v.currentTime : -1;
    });
    await page.waitForTimeout(3500);

    const info = await page.evaluate(() => {
      const v = document.querySelector('video');
      if (!v) return { found: false };
      const src = v.currentSrc || v.src || '';
      return {
        found: true,
        srcLen: src.length,
        srcHead: src.slice(0, 80),
        hasOpenlist: src.includes('/openlist/'),
        hasInternal: src.includes('127.0.0.1:5244') || src.includes('localhost:5244'),
        hasToken: src.includes('token='),
        readyState: v.readyState,
        networkState: v.networkState,
        error: v.error ? v.error.code : null,
        currentTime: v.currentTime,
        duration: isFinite(v.duration) ? Math.round(v.duration) : null,
      };
    });

    const reachedEnd = info.duration && info.currentTime >= info.duration - 0.6;
    const advanced = info.found && (info.currentTime > before + 0.1 || reachedEnd);
    const srcOk = c.expectOpenlist ? info.hasOpenlist : !info.hasInternal;
    const ok = info.found && info.error === null && info.readyState >= 2 && !info.hasInternal && advanced && !remuxStatus.some((s) => s.includes('401'));
    allOk = allOk && ok;

    log('   srcHead: ' + info.srcHead);
    log('   hasOpenlist=' + info.hasOpenlist + ' hasInternal=' + info.hasInternal + ' hasToken=' + info.hasToken);
    log('   readyState=' + info.readyState + ' error=' + info.error + ' currentTime=' + info.currentTime + ' advanced=' + advanced);
    log('   srcOk=' + srcOk + ' reached=' + reached);
    log('   => ' + (ok ? 'PASS ✅' : 'FAIL ❌'));
    await page.screenshot({ path: '/workspace/play_verify2_' + c.id + '.png' });
  }

  log('\n=== 汇总 ===');
  log('openlist/remux 响应: ' + JSON.stringify(remuxStatus));
  log('失败请求: ' + (failedRequests.length ? JSON.stringify(failedRequests.slice(0, 6)) : '无'));
  log('控制台错误: ' + (consoleErrors.length ? JSON.stringify(consoleErrors.slice(0, 6)) : '无'));
  log('页面异常: ' + (pageErrors.length ? JSON.stringify(pageErrors.slice(0, 6)) : '无'));
  log('=== 总判定: ' + (allOk ? 'ALL PASS ✅ 浏览器真实播放（MP4/MKV 修复生效）' : 'FAIL ❌') + ' ===');

  await browser.close();
  process.exit(allOk ? 0 : 1);
})().catch((e) => { console.error('SCRIPT ERROR:', e); process.exit(2); });
